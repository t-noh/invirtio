package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"invirtio/internal/bench"
	"invirtio/internal/harness"
)

func init() {
	register("bench-net", "benchmark the VDUSE virtio-net backend against the host's i40e", cmdBenchNet)
	register("bench-net-summary", "print the results of bench-net again", cmdBenchNetSummary)
	register("bench-perf-report", "write the reports of the perf profiles of a bench-net run", cmdBenchPerfReport)
}

const (
	// The namespace of the tested interface, on the host's i40e or
	// virtio_net.
	netBenchNS  = "vnet-bench"
	netHostAddr = "10.71.0.1"
	netPeerAddr = "10.71.0.2"
)

var netBenchTargets = []string{"native", "native-1q", "guest"}

// A netBenchJob is one measurement, run against every target. Omits is set
// for the jobs that leave out their first -omit seconds.
type netBenchJob struct {
	bench.Job
	run   func(s *netBenchSuite, ns string) (map[string]float64, error)
	omits bool
}

var netBenchJobs = []netBenchJob{
	{bench.Job{Name: "tcp-tx-1", About: "TCP to the far end, one stream", Metrics: []string{"gbps", "retransmits"}}, netBenchTCP(false, 1), true},
	{bench.Job{Name: "tcp-tx-4", About: "TCP to the far end, four streams", Metrics: []string{"gbps", "retransmits"}}, netBenchTCP(false, 4), true},
	{bench.Job{Name: "tcp-rx-1", About: "TCP from the far end, one stream", Metrics: []string{"gbps", "retransmits"}}, netBenchTCP(true, 1), true},
	{bench.Job{Name: "tcp-rx-4", About: "TCP from the far end, four streams", Metrics: []string{"gbps", "retransmits"}}, netBenchTCP(true, 4), true},
	{bench.Job{Name: "paced-tx-1g", About: "TCP to the far end, one stream paced at 1 Gb/s", Metrics: []string{"gbps", "retransmits"}}, netBenchPacedTCP(false, "1G"), true},
	{bench.Job{Name: "paced-rx-1g", About: "TCP from the far end, one stream paced at 1 Gb/s", Metrics: []string{"gbps", "retransmits"}}, netBenchPacedTCP(true, "1G"), true},
	{bench.Job{Name: "paced-tx-2.5g", About: "TCP to the far end, one stream paced at 2.5 Gb/s", Metrics: []string{"gbps", "retransmits"}}, netBenchPacedTCP(false, "2500M"), true},
	{bench.Job{Name: "paced-rx-2.5g", About: "TCP from the far end, one stream paced at 2.5 Gb/s", Metrics: []string{"gbps", "retransmits"}}, netBenchPacedTCP(true, "2500M"), true},
	{bench.Job{Name: "udp64-tx", About: "64-byte UDP datagrams to the far end at an unlimited rate", Metrics: []string{"sent_kpps", "received_kpps", "loss_pct"}}, netBenchUDP(false), true},
	{bench.Job{Name: "udp64-rx", About: "64-byte UDP datagrams from the far end at an unlimited rate", Metrics: []string{"sent_kpps", "received_kpps", "loss_pct"}}, netBenchUDP(true), true},
	{bench.Job{Name: "tcp-rr", About: "netperf TCP_RR with 1-byte messages", Metrics: []string{"tps", "mean_us", "p50_us", "p99_us"}}, netBenchRR("TCP_RR"), true},
	{bench.Job{Name: "udp-rr", About: "netperf UDP_RR with 1-byte messages", Metrics: []string{"tps", "mean_us", "p50_us", "p99_us"}}, netBenchRR("UDP_RR"), true},
	{bench.Job{Name: "ping-idle", About: "ping round trips on an idle link", Metrics: []string{"avg_us", "max_us", "mdev_us"}}, netBenchPing(false), false},
	{bench.Job{Name: "ping-load", About: "ping round trips during a TCP stream", Metrics: []string{"avg_us", "max_us", "mdev_us"}}, netBenchPing(true), false},
}

type netBenchSuite struct {
	out string
	// perfJob selects the jobs that are profiled, or is "". Their
	// profiles are reported at the end of a native run.
	perfJob  string
	profiles []*bench.Profile
	mtu      int
	seconds  int
	omit     int
	opts     *bench.Options
	jobs     []netBenchJob
	far      *netFarEnd
	results  bench.Results
	// enabled is port 0's guest for the guest target. guest is set to it
	// while the target runs, and its CPU use and memory are measured.
	enabled *harness.Enabled
	guest   *harness.Enabled
	memory  *harness.GuestMemory
	// ctx ends on a signal, which stops a running job, and the target that
	// runs then fails and cleans up.
	ctx context.Context
}

func cmdBenchNet(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("bench-net", `
usage: invirtio-test bench-net [options] [<target>...]

Benchmark the VDUSE virtio-net backend on the X710, whose two ports are
cabled to each other. Port 1 stays on the host's i40e in the network
namespace vnet-peer, with iperf3 and netserver, as the far end. Each target
is one of:
  native     port 0 on the host's i40e, with the queues that i40e chooses
             and their interrupts spread in turn over the CPUs outside
             the cores of -app-cpus and -peer-cpus
  native-1q  the same with one queue pair, as the guest's i40e has, and its
             interrupts on the device CPU
  guest      port 0 driven by the guest that invirtio enable started, and
             exported to the host's virtio_net

Each target's interface is in the namespace vnet-bench, where the jobs run.

The default is guest if port 0 is enabled, and both native targets
otherwise. The benchmark never changes port 0's driver, so a run has either
the guest target, for which port 0 must be enabled, or native targets, for
which it must be disabled and on i40e. Each round brings up every target
once and runs its jobs -reps times, and every other round runs the targets
in the reverse order. Both ports have an MTU of -mtu, and the guest's
backend takes one up to its mtu= option, which enable's -options sets. Each
throughput job runs for -time seconds after -omit seconds that it leaves
out, and each request/response job runs netperf for -omit seconds before
the -time seconds that it measures.

The benchmark's programs run on -app-cpus, and the far end's programs and
port 1's interrupts on -peer-cpus. With native-1q and the guest, the
device's interrupts go to -device-cpu, as do the guest's VDUSE interrupts,
and the guest must be pinned there, which enable's -irq-cpus and -args
cpu_pin= do. Each run records how many of the host's CPUs were busy and how
many of each set's, and with the guest, how many CPUs the guest's cgroup
took, how busy the guest's own CPU was, and how many DMA buffers it mapped
each second. During the run, every CPU uses the performance governor,
boost is as -boost says, and irqbalance is stopped. A summary's values are
means over the rounds.

With the guest, the results record the device's record and the guest's
kernel, the peak memory of the guest's cgroup since the guest started, and
the most memory that the guest itself used between jobs. Each round's
VDUSE and DMA counters count from the guest's start, with their values at
the start of the round under start_. With -perf, perf records the device
CPU, or every CPU without pinning, during each run of the matching jobs,
after the omitted seconds, into
perf-<job>-<target>-<round>-<rep>.data in the output directory. Its
messages go to .data.log. Reports by symbol go to .data.txt and by command
and object to .data.dso.txt, at the end of a native run, and for the
guest, which perf needs to have exited to find its symbols, with
bench-perf-report once the device is disabled. The output
directory gets results.json, runs.csv with one metric of one run on each
line, and the summary, which bench-net-summary prints again. Run as root
on the linux-host kernel, with iperf3, netperf and ethtool, and perf for
-perf.

Jobs:`+netBenchJobList())
	out := fs.String("out", filepath.Join(root, "build/bench-net"), "the output directory")
	seconds := fs.Int("time", 10, "the seconds that each throughput and request/response job measures")
	omit := fs.Int("omit", 2, "the seconds at the start of each throughput job, and before each request/response job, that it leaves out")
	mtu := fs.Int("mtu", 1500, "the MTU of both ports")
	perfJob := fs.String("perf", "", "profile the device CPU with perf during the jobs whose names contain this string")
	jobs := fs.String("jobs", "", "run only the jobs whose names contain this string")
	opts := bench.AddFlags(fs, 5, 1)
	fs.Parse(args)
	targets := fs.Args()
	for _, t := range targets {
		if !slices.Contains(netBenchTargets, t) {
			fs.Usage()
			os.Exit(2)
		}
	}
	if slices.Contains(targets, "guest") && len(targets) > 1 {
		return errors.New("the guest target needs port 0 enabled and the native targets need it disabled, so run them separately")
	}
	if err := opts.Check(); err != nil {
		return err
	}
	requireRoot()

	s := &netBenchSuite{seconds: *seconds, omit: *omit, mtu: *mtu, opts: opts, perfJob: *perfJob}
	for _, j := range netBenchJobs {
		if strings.Contains(j.Name, *jobs) {
			s.jobs = append(s.jobs, j)
		}
	}
	var err error
	if len(targets) == 0 {
		t, err := defaultTarget(netGuestPort, "guest", "native")
		if err != nil {
			return err
		}
		targets = []string{t}
		if t == "native" {
			targets = append(targets, "native-1q")
		}
	}
	if err := harness.Modprobe("i40e"); err != nil {
		return err
	}
	if targets[0] == "guest" {
		if s.enabled, err = harness.FindEnabled(netGuestPort); err != nil {
			return err
		}
		if err := s.enabled.CheckDeviceCPU(opts.DeviceCPU); err != nil {
			return err
		}
	} else if err := checkNative(netGuestPort); err != nil {
		return err
	}
	if s.out, err = filepath.Abs(*out); err != nil {
		return err
	}
	if err := os.MkdirAll(s.out, 0755); err != nil {
		return err
	}
	if err := harness.TeeOutput(filepath.Join(s.out, "bench.log")); err != nil {
		return err
	}
	tools := []string{"iperf3", "netperf", "netserver", "ethtool", "ping", "taskset"}
	if s.perfJob != "" {
		tools = append(tools, "perf")
	}
	for _, tool := range tools {
		if harness.Run("sh", "-c", "command -v "+tool).Code != 0 {
			return fmt.Errorf("%s is not installed", tool)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	s.ctx = ctx
	tuning, err := opts.Tune()
	if err != nil {
		return err
	}
	defer tuning.Restore()
	s.results.Env = s.env()
	s.results.Targets = targets
	if s.far, err = netStartPeer(netPeerAddr, s.mtu, opts.PeerCPUs, "iperf3", "netserver"); err != nil {
		return err
	}
	defer s.cleanup()
	defer s.report()

	for round := 1; round <= opts.Rounds; round++ {
		for _, name := range bench.Order(targets, round) {
			if ctx.Err() != nil {
				return errors.New("stopped by a signal")
			}
			fmt.Printf("== round %d: %s\n", round, name)
			// A run that was killed leaves the interface in the
			// namespace, which deleting it gives back.
			harness.Run("ip", "netns", "del", netBenchNS)
			in := bench.Instance{Target: name, Round: round}
			if name == "guest" {
				err = s.runGuest(&in)
			} else {
				err = s.runNative(&in, name == "native-1q")
			}
			if err != nil {
				return fmt.Errorf("round %d: %s: %v", round, name, err)
			}
			s.results.Instances = append(s.results.Instances, in)
			if err := s.results.Save(s.out); err != nil {
				return err
			}
		}
	}
	summary := s.results.Summary(netBenchJobDefs())
	fmt.Print(summary)
	return os.WriteFile(filepath.Join(s.out, "summary.txt"), []byte(summary), 0644)
}

func netBenchJobList() string {
	var b strings.Builder
	for _, j := range netBenchJobs {
		fmt.Fprintf(&b, "\n  %-13s %s", j.Name, j.About)
	}
	return b.String()
}

func netBenchJobDefs() []bench.Job {
	var jobs []bench.Job
	for _, j := range netBenchJobs {
		jobs = append(jobs, j.Job)
	}
	return jobs
}

func (s *netBenchSuite) env() map[string]string {
	env := bench.Environment()
	for k, v := range s.opts.Describe() {
		env[k] = v
	}
	env["time"] = strconv.Itoa(s.seconds)
	env["omit"] = strconv.Itoa(s.omit)
	env["mtu"] = strconv.Itoa(s.mtu)
	if s.enabled != nil {
		s.enabled.Describe(env)
	}
	env["perf"] = s.perfJob
	env["iperf3"] = strings.SplitN(harness.Run("iperf3", "--version").Stdout, "\n", 2)[0]
	env["netperf"] = strings.TrimSpace(harness.Run("netperf", "-V").Stdout)
	env["nic"] = strings.TrimSpace(harness.Run("lspci", "-s", netGuestPort).Stdout)
	return env
}

// report writes the reports of the profiles, unless a guest runs, whose
// symbols perf finds only once it has exited.
func (s *netBenchSuite) report() {
	if len(s.profiles) == 0 {
		return
	}
	if s.enabled != nil {
		fmt.Printf("Write the profiles' reports once %s is disabled with: invirtio-test bench-perf-report %s\n",
			s.enabled.Name, s.out)
		return
	}
	for _, p := range s.profiles {
		if err := p.Report(); err != nil {
			fmt.Println("warning:", err)
		}
	}
}

func (s *netBenchSuite) cleanup() {
	harness.Run("ip", "netns", "del", netBenchNS)
	s.far.stop()
}

// carrierIn waits for an interface in a namespace to have carrier.
func carrierIn(ns, net string) bool {
	return harness.Wait(func() bool {
		return strings.TrimSpace(harness.RunArgv(harness.InNS(ns, "cat", "/sys/class/net/"+net+"/carrier")).Stdout) == "1"
	}, 30*time.Second, 200*time.Millisecond)
}

// netInfo describes an interface's driver, queues, rings and offloads.
func netInfo(ns, net string) map[string]string {
	info := map[string]string{}
	for _, opt := range []string{"-i", "-l", "-g", "-k"} {
		info["ethtool "+opt] = strings.TrimSpace(harness.RunArgv(harness.InNS(ns, "ethtool", opt, net)).Stdout)
	}
	return info
}

var ethtoolCombinedRE = regexp.MustCompile(`(?s)Current hardware settings:.*?Combined:\s*(\d+)`)

// enterNS takes an interface from NetworkManager, moves it into netBenchNS
// and brings it up with the host's address, and returns a function that
// moves it back with its MTU, gives it back to NetworkManager if it had it,
// and removes the namespace.
func (s *netBenchSuite) enterNS(net string) (func(), error) {
	harness.Run("ip", "netns", "del", netBenchNS)
	mtu := harness.Read(filepath.Join("/sys/class/net", net, "mtu"))
	managed := harness.Managed(net)
	harness.Release(net)
	leave := func() {
		harness.RunArgv(harness.InNS(netBenchNS, "ip", "link", "set", net, "netns", "1"))
		harness.Run("ip", "netns", "del", netBenchNS)
		harness.Run("ip", "link", "set", net, "mtu", mtu)
		if managed {
			harness.Run("nmcli", "device", "set", net, "managed", "yes")
		}
	}
	for _, argv := range [][]string{
		{"ip", "netns", "add", netBenchNS},
		{"ip", "link", "set", net, "netns", netBenchNS},
		harness.InNS(netBenchNS, "ip", "link", "set", net, "mtu", fmt.Sprint(s.mtu), "up"),
		harness.InNS(netBenchNS, "ip", "addr", "add", netHostAddr+"/24", "dev", net),
	} {
		if err := harness.MustRun(argv[0], argv[1:]...); err != nil {
			leave()
			return nil, err
		}
	}
	return leave, nil
}

// runNative runs the jobs with port 0 on the host's i40e, in netBenchNS.
// With oneQueue, the port has one queue pair, and its interrupts go to the
// device CPU, as the guest's do.
func (s *netBenchSuite) runNative(in *bench.Instance, oneQueue bool) error {
	port := harness.WaitNetdev(netGuestPort, 10*time.Second)
	if port == "" {
		return fmt.Errorf("%s has no interface on the host", netGuestPort)
	}
	leave, err := s.enterNS(port)
	if err != nil {
		return err
	}
	defer leave()
	ns := func(argv ...string) []string { return harness.InNS(netBenchNS, argv...) }
	if oneQueue {
		m := ethtoolCombinedRE.FindStringSubmatch(harness.RunArgv(ns("ethtool", "-l", port)).Stdout)
		if m == nil {
			return fmt.Errorf("ethtool shows no combined queues for %s", port)
		}
		argv := ns("ethtool", "-L", port, "combined", "1")
		if err := harness.MustRun(argv[0], argv[1:]...); err != nil {
			return err
		}
		defer harness.RunArgv(ns("ethtool", "-L", port, "combined", m[1]))
	}
	// The port's interrupts stay off the cores of the benchmark's programs
	// and the far end, as the guest's do.
	if other := s.opts.OtherCPUs(); other != "" {
		cpus := strings.Split(other, ",")
		if oneQueue {
			cpus = []string{s.opts.DeviceCPU}
		}
		restore, err := bench.SteerIRQs(netGuestPort, cpus...)
		if err != nil {
			return err
		}
		defer restore()
	}
	if !carrierIn(netBenchNS, port) {
		return fmt.Errorf("%s has no carrier", port)
	}
	in.Info = netInfo(netBenchNS, port)
	return s.runJobs(in, netBenchNS)
}

// runGuest runs the jobs through the interface of port 0's guest.
func (s *netBenchSuite) runGuest(in *bench.Instance) error {
	e := s.enabled
	// A guest that failed since the last round fails this one.
	if _, err := harness.FindEnabled(e.Device.Address); err != nil {
		return err
	}
	// An interface that a killed run left in the namespace comes back a
	// moment after the namespace is deleted.
	var nets []string
	harness.Wait(func() bool {
		nets = harness.VirtioNetdevs(e.Name)
		return len(nets) > 0
	}, 10*time.Second, 100*time.Millisecond)
	if len(nets) == 0 {
		return errors.New("the host has no interface for " + e.Name)
	}
	net := nets[0]
	cpu := -1
	if s.opts.DeviceCPU != "" {
		cpu, _ = strconv.Atoi(s.opts.DeviceCPU)
	}
	defer harness.SaveIRQs(e.Name)()
	if err := harness.PlaceIRQs(e.Name, cpu); err != nil {
		return err
	}
	leave, err := s.enterNS(net)
	if err != nil {
		return err
	}
	defer leave()
	if !carrierIn(netBenchNS, net) {
		return fmt.Errorf("%s has no carrier", net)
	}
	in.Info = netInfo(netBenchNS, net)
	s.guest = e
	defer func() { s.guest, s.memory = nil, nil }()
	s.memory = &harness.GuestMemory{Mem: e.Device.GuestMemory()}
	s.memory.SampleGuest(e.Proc("meminfo"))
	start := guestCounters(e, nil)
	if err := s.runJobs(in, netBenchNS); err != nil {
		return err
	}
	in.Counters = guestCounters(e, start)
	if err := s.memory.ReadCgroup(e.Cgroup); err != nil {
		return err
	}
	in.Memory = s.memory
	fmt.Println("  memory: " + in.Memory.Summary())
	return nil
}

// guestCPU returns the busy and total ticks of the guest's CPU.
func (s *netBenchSuite) guestCPU() (int64, int64) {
	line, _, _ := strings.Cut(s.guest.Proc("stat"), "\n")
	return harness.ParseCPULine(strings.TrimSpace(line))
}

// runJobs runs each job -reps times from the namespace ns, with the CPU
// use of each run and the guest's DMA mappings.
func (s *netBenchSuite) runJobs(in *bench.Instance, ns string) error {
	for _, j := range s.jobs {
		for rep := 1; rep <= s.opts.Reps; rep++ {
			if s.ctx.Err() != nil {
				return errors.New("stopped by a signal")
			}
			var gbusy0, gtotal0 int64
			var dma0 map[string]int64
			if s.guest != nil {
				dma0 = s.guest.DMACounters()
				gbusy0, gtotal0 = s.guestCPU()
			}
			var prof *bench.Profile
			if s.perfJob != "" && strings.Contains(j.Name, s.perfJob) {
				data := filepath.Join(s.out, fmt.Sprintf("perf-%s-%s-%d-%d.data", j.Name, in.Target, in.Round, rep))
				var delay time.Duration
				if j.omits {
					delay = time.Duration(s.omit) * time.Second
				}
				var err error
				prof, err = bench.StartProfile(s.ctx, s.opts.DeviceCPU, data, delay,
					time.Duration(s.seconds+60)*time.Second)
				if err != nil {
					return fmt.Errorf("%s: %v", j.Name, err)
				}
			}
			start := time.Now()
			var cgroup string
			if s.guest != nil {
				cgroup = s.guest.Cgroup
			}
			meter := bench.StartMeter(s.opts.Sets(), cgroup)
			m, err := j.run(s, ns)
			cpu := meter.Stop()
			elapsed := time.Since(start)
			if prof != nil {
				if perr := prof.Stop(); perr != nil && err == nil {
					err = perr
				}
				s.profiles = append(s.profiles, prof)
			}
			if err != nil {
				return fmt.Errorf("%s: %v", j.Name, err)
			}
			for k, v := range cpu {
				m[k] = v
			}
			if s.guest != nil {
				gbusy1, gtotal1 := s.guestCPU()
				if gtotal1 > gtotal0 && gtotal0 > 0 {
					m["guest_busy"] = float64(gbusy1-gbusy0) / float64(gtotal1-gtotal0)
				}
				for k, v := range bench.DMARates(dma0, s.guest.DMACounters(), elapsed) {
					m[k] = v
				}
				s.memory.SampleGuest(s.guest.Proc("meminfo"))
			}
			fmt.Printf("  %s %d: %s\n", j.Name, rep, bench.Format(j.Job, m))
			s.results.Runs = append(s.results.Runs, bench.Run{Job: j.Name, Target: in.Target, Round: in.Round, Rep: rep, Metrics: m})
		}
	}
	return nil
}

// run runs a command on the application CPUs in the namespace ns, which a
// signal stops.
func (s *netBenchSuite) run(ns string, timeout time.Duration, argv ...string) harness.Result {
	argv = harness.InNS(ns, bench.Pin(s.opts.AppCPUs, argv...)...)
	return harness.RunContext(s.ctx, s.out, timeout, argv[0], argv[1:]...)
}

func (s *netBenchSuite) iperf(ns string, args ...string) (harness.Iperf, error) {
	argv := append([]string{"iperf3", "-c", netPeerAddr, "-J", "-t", fmt.Sprint(s.seconds), "-O", fmt.Sprint(s.omit)}, args...)
	return harness.ParseIperf(s.run(ns, time.Duration(s.seconds+s.omit+60)*time.Second, argv...).Stdout)
}

func netBenchTCP(reverse bool, streams int) func(*netBenchSuite, string) (map[string]float64, error) {
	return func(s *netBenchSuite, ns string) (map[string]float64, error) {
		args := []string{"-P", fmt.Sprint(streams)}
		if reverse {
			args = append(args, "-R")
		}
		r, err := s.iperf(ns, args...)
		if err != nil {
			return nil, err
		}
		return map[string]float64{"gbps": r.ReceivedBPS / 1e9, "retransmits": float64(r.Retransmits)}, nil
	}
}

// netBenchPacedTCP runs one TCP stream whose sender's socket the kernel
// paces at rate. The kernel paces whole TSO packets, so the data still
// leaves in bursts at line rate, but in smaller ones than iperf3's own
// pacing timer sends.
func netBenchPacedTCP(reverse bool, rate string) func(*netBenchSuite, string) (map[string]float64, error) {
	return func(s *netBenchSuite, ns string) (map[string]float64, error) {
		args := []string{"--fq-rate", rate}
		if reverse {
			args = append(args, "-R")
		}
		r, err := s.iperf(ns, args...)
		if err != nil {
			return nil, err
		}
		return map[string]float64{"gbps": r.ReceivedBPS / 1e9, "retransmits": float64(r.Retransmits)}, nil
	}
}

func netBenchUDP(reverse bool) func(*netBenchSuite, string) (map[string]float64, error) {
	return func(s *netBenchSuite, ns string) (map[string]float64, error) {
		args := []string{"-u", "-b", "0", "-l", "64"}
		if reverse {
			args = append(args, "-R")
		}
		r, err := s.iperf(ns, args...)
		if err != nil {
			return nil, err
		}
		if r.Seconds <= 0 || r.Packets <= 0 {
			return nil, errors.New("iperf3 reported no datagrams")
		}
		return map[string]float64{
			"sent_kpps":     float64(r.Packets) / r.Seconds / 1e3,
			"received_kpps": float64(r.Packets-r.LostPackets) / r.Seconds / 1e3,
			"loss_pct":      100 * float64(r.LostPackets) / float64(r.Packets),
		}, nil
	}
}

// netperfRRKeys are the metrics of netperfRROutput's selectors, in order.
var netperfRRKeys = []string{"tps", "mean_us", "p50_us", "p99_us"}

const netperfRROutput = "THROUGHPUT,MEAN_LATENCY,P50_LATENCY,P99_LATENCY"

// netBenchRR runs netperf for the -omit seconds of warm-up, whose results
// it drops, and then for the -time seconds that it measures.
func netBenchRR(test string) func(*netBenchSuite, string) (map[string]float64, error) {
	return func(s *netBenchSuite, ns string) (map[string]float64, error) {
		rr := func(seconds int) (map[string]float64, error) {
			r := s.run(ns, time.Duration(seconds+60)*time.Second, "netperf", "-H", netPeerAddr, "-t", test,
				"-l", fmt.Sprint(seconds), "-P", "0", "--", "-r", "1,1", "-o", netperfRROutput)
			return parseNetperf(r.Stdout, r.Output())
		}
		if s.omit > 0 {
			if _, err := rr(s.omit); err != nil {
				return nil, fmt.Errorf("warm-up: %v", err)
			}
		}
		return rr(s.seconds)
	}
}

// parseNetperf parses the last line of netperf's -o output.
func parseNetperf(stdout, all string) (map[string]float64, error) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	f := strings.Split(lines[len(lines)-1], ",")
	if len(f) != len(netperfRRKeys) {
		return nil, fmt.Errorf("netperf printed %q", strings.TrimSpace(all))
	}
	m := map[string]float64{}
	for i, k := range netperfRRKeys {
		v, err := strconv.ParseFloat(strings.TrimSpace(f[i]), 64)
		if err != nil {
			return nil, fmt.Errorf("netperf printed %q", strings.TrimSpace(all))
		}
		m[k] = v
	}
	return m, nil
}

func netBenchPing(load bool) func(*netBenchSuite, string) (map[string]float64, error) {
	return func(s *netBenchSuite, ns string) (map[string]float64, error) {
		if load {
			ctx, cancel := context.WithCancel(s.ctx)
			done := make(chan struct{})
			go func() {
				argv := harness.InNS(ns, bench.Pin(s.opts.AppCPUs, "iperf3", "-c", netPeerAddr, "-t", "60")...)
				harness.RunContext(ctx, s.out, 2*time.Minute, argv[0], argv[1:]...)
				close(done)
			}()
			defer func() {
				cancel()
				<-done
			}()
			time.Sleep(2 * time.Second)
		}
		r := s.run(ns, time.Minute, "ping", "-q", "-c", "100", "-i", "0.05", netPeerAddr)
		p, ok := harness.ParsePing(r.Output())
		if !ok || p.Received == 0 {
			return nil, fmt.Errorf("ping printed %q", strings.TrimSpace(r.Output()))
		}
		return map[string]float64{"avg_us": p.Avg * 1000, "max_us": p.Max * 1000, "mdev_us": p.Mdev * 1000}, nil
	}
}

func cmdBenchNetSummary(args []string) error {
	fs := newFlags("bench-net-summary", `
usage: invirtio-test bench-net-summary <output dir>

Print the summary of a bench-net run from its results.json.`)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	r, err := bench.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Print(r.Summary(netBenchJobDefs()))
	return nil
}

func cmdBenchPerfReport(args []string) error {
	fs := newFlags("bench-perf-report", `
usage: invirtio-test bench-perf-report <output dir>

Write the reports of each perf profile of a bench-net run, perf-*.data, by
symbol in .data.txt and by command and object in .data.dso.txt. Perf finds
the symbols of a guest only once it has exited, so run this once the
device is disabled.`)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	files, _ := filepath.Glob(filepath.Join(fs.Arg(0), "perf-*.data"))
	if len(files) == 0 {
		return fmt.Errorf("%s has no perf profiles", fs.Arg(0))
	}
	for _, f := range files {
		if err := bench.Report(f); err != nil {
			return err
		}
		fmt.Println(f + ".txt")
	}
	return nil
}
