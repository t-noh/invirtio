package bench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A Profile records host CPUs with perf while a job runs.
type Profile struct {
	cmd  *exec.Cmd
	data string
	log  *os.File
}

// StartProfile records the CPUs, or all of them for "", with call graphs,
// from delay after the start until Stop, and at most for limit, into the
// file data. Perf's messages go to <data>.log. Perf runs in a process group
// of its own and is stopped with SIGINT, so that it finishes the file.
func StartProfile(ctx context.Context, cpus, data string, delay, limit time.Duration) (*Profile, error) {
	log, err := os.Create(data + ".log")
	if err != nil {
		return nil, err
	}
	args := []string{"record", "-g", "-F", "999", "-o", data}
	if ms := delay.Milliseconds(); ms > 0 {
		args = append(args, "-D", fmt.Sprint(ms))
	}
	if cpus == "" {
		args = append(args, "-a")
	} else {
		args = append(args, "-C", cpus)
	}
	args = append(args, "--", "sleep", fmt.Sprintf("%.3f", (delay+limit).Seconds()))
	cmd := exec.CommandContext(ctx, "perf", args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = time.Minute
	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	return &Profile{cmd: cmd, data: data, log: log}, nil
}

// Stop ends the recording and waits for perf to finish the file. Perf
// stops its sleep with SIGTERM and then exits by that signal, or by SIGINT
// if it was stopped before it handled signals.
func (p *Profile) Stop() error {
	defer p.log.Close()
	p.cmd.Process.Signal(syscall.SIGINT)
	err := p.cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		ws, ok := exit.Sys().(syscall.WaitStatus)
		if ok && ws.Signaled() &&
			(ws.Signal() == syscall.SIGTERM || ws.Signal() == syscall.SIGINT) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("perf record: %v, see %s.log", err, p.data)
	}
	return os.Chmod(p.data, 0644)
}

// Report writes the reports of the recording, as the function Report does.
func (p *Profile) Report() error {
	return Report(p.data)
}

// Report writes two reports of a recording, by symbol in <data>.txt and by
// command and object in <data>.dso.txt, which any user can read. Perf looks
// a process's files up in its mount namespace while the process runs, so a
// guest's symbols only resolve once the guest has exited.
func Report(data string) error {
	for _, r := range []struct {
		file string
		args []string
	}{
		{data + ".txt", []string{"--sort", "dso,sym"}},
		{data + ".dso.txt", []string{"--sort", "comm,dso", "-g", "none"}},
	} {
		f, err := os.Create(r.file)
		if err != nil {
			return err
		}
		args := append([]string{"report", "-i", data, "--stdio", "--no-children",
			"--percent-limit", "0.5"}, r.args...)
		cmd := exec.Command("perf", args...)
		cmd.Stdout, cmd.Stderr = f, f
		err = cmd.Run()
		f.Close()
		if err != nil {
			return fmt.Errorf("perf report: %v, see %s", err, r.file)
		}
		os.Chmod(r.file, 0644)
	}
	return nil
}

// CycleKeys are the metrics of a Counter: the billions of cycles that each
// set's CPUs ran per second of the run, and their instructions per cycle.
var CycleKeys = []string{"gcycles_app", "gcycles_device", "gcycles_aux", "gcycles_peer", "ipc_app", "ipc_device", "ipc_aux"}

// A Counter counts the cycles and instructions of the CPUs of sets of host
// CPUs with perf stat.
type Counter struct {
	cmd  *exec.Cmd
	out  string
	sets []Set
}

// StartCounter counts the CPUs of the sets into the file out, in perf
// stat's CSV form with a line for each CPU and event, until Stop. Perf runs
// without a workload, so SIGINT ends its counting.
func StartCounter(ctx context.Context, sets []Set, out string) (*Counter, error) {
	var cpus []string
	for _, s := range sets {
		for _, c := range s.CPUs {
			cpus = append(cpus, strconv.Itoa(c))
		}
	}
	if len(cpus) == 0 {
		return nil, errors.New("perf stat needs pinned CPUs")
	}
	cmd := exec.CommandContext(ctx, "perf", "stat", "-A", "-x,", "-e", "cycles,instructions",
		"-C", strings.Join(cpus, ","), "-o", out)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = time.Minute
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Counter{cmd: cmd, out: out, sets: sets}, nil
}

// Stop ends the counting and returns the CycleKeys metrics of the sets that
// were counted, per second of elapsed.
func (c *Counter) Stop(elapsed time.Duration) (map[string]float64, error) {
	c.cmd.Process.Signal(syscall.SIGINT)
	err := c.cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGINT {
			err = nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("perf stat: %v, see %s", err, c.out)
	}
	data, err := os.ReadFile(c.out)
	if err != nil {
		return nil, err
	}
	counts := parsePerfStat(string(data))
	out := map[string]float64{}
	if elapsed <= 0 {
		return out, nil
	}
	for _, s := range c.sets {
		var cycles, insns float64
		for _, cpu := range s.CPUs {
			cycles += counts[cpu]["cycles"]
			insns += counts[cpu]["instructions"]
		}
		out["gcycles_"+s.Name] = cycles / 1e9 / elapsed.Seconds()
		if cycles > 0 && s.Name != "peer" {
			out["ipc_"+s.Name] = insns / cycles
		}
	}
	return out, nil
}

// parsePerfStat returns the counts of perf stat -A -x, output by CPU and
// event. Lines are CPU<n>,<count>,<unit>,<event>,... and events that were
// not counted have a count that is not a number.
func parsePerfStat(text string) map[int]map[string]float64 {
	counts := map[int]map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(line, ",")
		if len(f) < 4 || !strings.HasPrefix(f[0], "CPU") {
			continue
		}
		cpu, err := strconv.Atoi(f[0][3:])
		if err != nil {
			continue
		}
		n, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		event, _, _ := strings.Cut(f[3], ":")
		if counts[cpu] == nil {
			counts[cpu] = map[string]float64{}
		}
		counts[cpu][event] = n
	}
	return counts
}
