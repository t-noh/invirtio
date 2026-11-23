package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	"invirtio/internal/pci"
	"invirtio/internal/record"
)

func init() {
	register("bench-wlan", "benchmark a WiFi card through the guest against its host driver", cmdBenchWlan)
	register("bench-wlan-summary", "print the results of bench-wlan again", cmdBenchWlanSummary)
}

const (
	wlanBenchNS   = "wlan-bench"
	wlanBenchPort = 5299
)

var wlanBenchTargets = []string{"native", "guest"}

var wlanBenchJobs = []bench.Job{
	{Name: "scan", About: "a scan with iw, before joining", Metrics: []string{"seconds", "bss"}},
	{Name: "connect", About: "from the start of wpa_supplicant until it completes the connection", Metrics: []string{"seconds"}},
	{Name: "upload", About: "iperf3 TCP to the server", Metrics: []string{"mbps", "retransmits"}},
	{Name: "download", About: "iperf3 TCP from the server", Metrics: []string{"mbps", "retransmits"}},
	{Name: "ping-idle", About: "50 pings of the server at 200 ms on an idle link", Metrics: []string{"avg_ms", "max_ms", "mdev_ms", "loss_pct"}},
	{Name: "ping-load", About: "pings of the server at 200 ms during a download", Metrics: []string{"avg_ms", "max_ms", "mdev_ms", "loss_pct"}},
}

// wlanBenchUdhcpc is the script of the DHCP client in the namespace, which
// adds the address and a default route.
const wlanBenchUdhcpc = `#!/bin/sh
case "$1" in
bound|renew)
	ip addr flush dev "$interface"
	ip addr add "$ip/$mask" dev "$interface"
	[ -n "$router" ] && ip route replace default via "${router%% *}"
	;;
esac
exit 0
`

type wlanBenchSuite struct {
	out      string
	card     string
	conf     string
	server   string
	noServer bool
	seconds  int
	omit     int
	opts     *bench.Options
	results  bench.Results
	// enabled is the card's guest for the guest target. guest is set to it
	// while the target runs, and its memory is sampled between runs and
	// its cgroup's CPU use is measured.
	enabled *harness.Enabled
	guest   *harness.Enabled
	memory  *harness.GuestMemory
	// ctx ends on a signal, which stops a running iperf3, and the target
	// that runs then fails and cleans up.
	ctx context.Context
}

func cmdBenchWlan(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("bench-wlan", `
usage: invirtio-test bench-wlan [options] -conf <wpa_supplicant.conf> -server <address> [<target>]

Measure a WiFi card's throughput, latency, scan and connection time against
an iperf3 server that is reached through the access point. The target is
one of:
  native   the card on its host driver
  guest    the card driven by the guest that invirtio enable started, and
           exported to the host's virtio_wlan

The default is guest if the card is enabled, and native otherwise. The
benchmark never changes the card's driver. For the guest target, the card
must be enabled, and for the native target, the card must be disabled and
on its host driver, which mt7921e takes once it is loaded. Each round
measures the target once.

For each round, the interface is taken from NetworkManager and the system
wpa_supplicant, and its wiphy moves into the network namespace wlan-bench,
so that traffic to a server on this host crosses the radio instead of being
delivered locally. There power save is turned off, the card scans once
with iw, and it joins the network of -conf with wpa_supplicant and gets a
lease with busybox udhcpc. The server is one of this host's addresses on
the access point's uplink, where bench-wlan runs an iperf3 server on port
5299 unless -no-server is given, or a host behind the access point that
runs one. Each round then runs the jobs -reps times, each iperf3 run for
-time seconds after -omit seconds that it leaves out.

The benchmark's programs run on -app-cpus, and the iperf3 server on
-peer-cpus. The card's interrupts and the guest's VDUSE interrupts go to
-device-cpu, where the guest must be pinned, which enable's -irq-cpus and
-args cpu_pin= do. Each run records how many of the host's CPUs were busy
and how many of each set's, and with the guest, how many CPUs the guest's
cgroup took and how many DMA buffers it mapped each second. During the run,
every CPU uses the performance governor, boost is as -boost says, and
irqbalance is stopped.

With the guest, the results record the device's record and the guest's
kernel, the peak memory of the guest's cgroup since the guest started, and
the most memory that the guest itself used between runs. Each round's
VDUSE and DMA counters count from the guest's start, with their values at
the start of the round under start_. The output directory gets results.json,
runs.csv with one metric of one run on each line, the summary, which
bench-wlan-summary prints again, and the logs. Run as root on the
linux-host kernel, with iw, wpa_supplicant, iperf3 and busybox.

Jobs:`+wlanBenchJobList())
	out := fs.String("out", filepath.Join(root, "build/bench-wlan"), "the output directory")
	card := fs.String("device", "0000:06:00.0", "the PCI address of the card")
	conf := fs.String("conf", "", "the wpa_supplicant configuration of the network")
	server := fs.String("server", "", "the address of the iperf3 server")
	noServer := fs.Bool("no-server", false, "use an iperf3 server that already runs")
	seconds := fs.Int("time", 10, "the seconds that each iperf3 run measures")
	omit := fs.Int("omit", 2, "the seconds at the start of each iperf3 run that it leaves out")
	opts := bench.AddFlags(fs, 5, 2)
	fs.Parse(args)
	if fs.NArg() > 1 || (fs.NArg() == 1 && !slices.Contains(wlanBenchTargets, fs.Arg(0))) {
		fs.Usage()
		os.Exit(2)
	}
	if *conf == "" || *server == "" {
		fs.Usage()
		os.Exit(2)
	}
	if err := opts.Check(); err != nil {
		return err
	}
	requireRoot()

	s := &wlanBenchSuite{conf: *conf, server: *server, noServer: *noServer,
		seconds: *seconds, omit: *omit, opts: opts}
	var err error
	if s.card, err = pci.NormalizeAddress(*card); err != nil {
		return err
	}
	target := fs.Arg(0)
	if target == "" {
		if target, err = defaultTarget(s.card, "guest", "native"); err != nil {
			return err
		}
	}
	if target == "guest" {
		if s.enabled, err = harness.FindEnabled(s.card); err != nil {
			return err
		}
		if err := s.enabled.CheckDeviceCPU(opts.DeviceCPU); err != nil {
			return err
		}
	} else if err := checkNative(s.card); err != nil {
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
	for _, tool := range []string{"iw", "wpa_supplicant", "wpa_cli", "iperf3", "busybox", "ping", "taskset", "ethtool"} {
		if harness.Run("sh", "-c", "command -v "+tool).Code != 0 {
			return fmt.Errorf("%s is not installed", tool)
		}
	}
	if err := os.WriteFile(filepath.Join(s.out, "udhcpc.sh"), []byte(wlanBenchUdhcpc), 0755); err != nil {
		return err
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
	s.results.Targets = []string{target}
	for round := 1; round <= opts.Rounds; round++ {
		if ctx.Err() != nil {
			return errors.New("stopped by a signal")
		}
		fmt.Printf("== round %d: %s\n", round, target)
		// A run that was killed leaves the wiphy in the namespace, which
		// deleting it gives back.
		harness.Run("ip", "netns", "del", wlanBenchNS)
		in := bench.Instance{Target: target, Round: round}
		if target == "native" {
			err = s.runNative(&in)
		} else {
			err = s.runGuest(&in)
		}
		if err != nil {
			return fmt.Errorf("round %d: %s: %v", round, target, err)
		}
		s.results.Instances = append(s.results.Instances, in)
		if err := s.results.Save(s.out); err != nil {
			return err
		}
	}
	summary := wlanBenchSummary(&s.results)
	fmt.Print(summary)
	return os.WriteFile(filepath.Join(s.out, "summary.txt"), []byte(summary), 0644)
}

// guestCounters returns a guest's VDUSE counters and its DMA counters with
// the prefix dma_. They count from the guest's start, so the counters of
// start, if given, are added with the prefix start_.
func guestCounters(e *harness.Enabled, start map[string]int64) map[string]int64 {
	c := e.Counters()
	for k, v := range e.DMACounters() {
		c["dma_"+k] = v
	}
	for k, v := range start {
		c["start_"+k] = v
	}
	return c
}

// defaultTarget returns guest if a device is enabled, and native otherwise.
func defaultTarget(addr, guest, native string) (string, error) {
	_, _, err := record.Find(addr)
	if err == nil {
		return guest, nil
	}
	if errors.Is(err, record.ErrNotEnabled) {
		return native, nil
	}
	return "", err
}

// checkNative checks that a device is on its host driver and not enabled,
// for a benchmark's native target.
func checkNative(addr string) error {
	name, _, err := record.Find(addr)
	if err == nil {
		return fmt.Errorf("%s is enabled as %s, so disable it first with: invirtio disable %s", addr, name, name)
	}
	if !errors.Is(err, record.ErrNotEnabled) {
		return err
	}
	switch drv := harness.PCIDriver(addr); drv {
	case "":
		return fmt.Errorf("%s has no driver, so load its host driver first", addr)
	case "vfio-pci":
		return fmt.Errorf("%s is on vfio-pci, so give it back to its host driver first", addr)
	}
	return nil
}

func wlanBenchJobList() string {
	var b strings.Builder
	for _, j := range wlanBenchJobs {
		fmt.Fprintf(&b, "\n  %-10s %s", j.Name, j.About)
	}
	return b.String()
}

func (s *wlanBenchSuite) env() map[string]string {
	env := bench.Environment()
	for k, v := range s.opts.Describe() {
		env[k] = v
	}
	env["card"] = strings.TrimSpace(harness.Run("lspci", "-s", s.card).Stdout)
	env["server"] = s.server
	env["time"] = strconv.Itoa(s.seconds)
	env["omit"] = strconv.Itoa(s.omit)
	if s.enabled != nil {
		s.enabled.Describe(env)
	}
	env["iperf3"] = strings.SplitN(harness.Run("iperf3", "--version").Stdout, "\n", 2)[0]
	env["wpa_supplicant"] = strings.TrimSpace(harness.Run("wpa_supplicant", "-v").Stdout)
	return env
}

// wiphy waits for a wiphy and an interface under a device directory, and
// for udev to rename the interface.
func wiphy(dir string) (phy, net string) {
	find := func() bool {
		phys := harness.Glob(filepath.Join(dir, "ieee80211/*"))
		nets := harness.Glob(filepath.Join(dir, "net/*"))
		phy, net = "", ""
		if len(phys) > 0 && len(nets) > 0 {
			phy, net = phys[0], nets[0]
		}
		return net != ""
	}
	if harness.Wait(find, 30*time.Second, 100*time.Millisecond) {
		harness.Run("udevadm", "settle")
		find()
	}
	return phy, net
}

// runNative brings the card up on its host driver.
func (s *wlanBenchSuite) runNative(in *bench.Instance) error {
	phy, net := wiphy(pci.Path(s.card, ""))
	if net == "" {
		return fmt.Errorf("%s's driver %q made no wiphy", s.card, harness.PCIDriver(s.card))
	}
	if s.opts.DeviceCPU != "" {
		restore, err := bench.SteerIRQs(s.card, s.opts.DeviceCPU)
		if err != nil {
			return err
		}
		defer restore()
	}
	return s.measure(in, phy, net)
}

// runGuest brings up the interface of the card's guest.
func (s *wlanBenchSuite) runGuest(in *bench.Instance) error {
	e := s.enabled
	// A guest that failed since the last round fails this one.
	if _, err := harness.FindEnabled(e.Device.Address); err != nil {
		return err
	}
	devs, _ := filepath.Glob(filepath.Join("/sys/bus/vdpa/devices", e.Name, "virtio*"))
	if len(devs) == 0 {
		return errors.New("virtio_vdpa has no virtio device for " + e.Name)
	}
	phy, net := wiphy(devs[0])
	if net == "" {
		return errors.New("virtio_wlan made no wiphy")
	}
	cpu := -1
	if s.opts.DeviceCPU != "" {
		cpu, _ = strconv.Atoi(s.opts.DeviceCPU)
	}
	defer harness.SaveIRQs(e.Name)()
	if err := harness.PlaceIRQs(e.Name, cpu); err != nil {
		return err
	}
	s.guest = e
	defer func() { s.guest, s.memory = nil, nil }()
	s.memory = &harness.GuestMemory{Mem: e.Device.GuestMemory()}
	s.memory.SampleGuest(e.Proc("meminfo"))
	start := guestCounters(e, nil)
	if err := s.measure(in, phy, net); err != nil {
		return err
	}
	in.Counters = guestCounters(e, start)
	s.memory.SampleGuest(e.Proc("meminfo"))
	if err := s.memory.ReadCgroup(e.Cgroup); err != nil {
		return err
	}
	in.Memory = s.memory
	fmt.Println("  memory: " + in.Memory.Summary())
	return nil
}

var (
	wlanBSSRE       = regexp.MustCompile(`(?m)^BSS `)
	wlanPowerSaveRE = regexp.MustCompile(`Power save: (\w+)`)
)

// measure joins the network in the namespace and runs the jobs.
func (s *wlanBenchSuite) measure(in *bench.Instance, phy, net string) error {
	dir := filepath.Join(s.out, fmt.Sprintf("%s-%d", in.Target, in.Round))
	os.MkdirAll(dir, 0755)
	ns := func(argv ...string) []string { return harness.InNS(wlanBenchNS, argv...) }
	app := func(argv ...string) []string { return ns(bench.Pin(s.opts.AppCPUs, argv...)...) }
	// The interface outlives the benchmark, so it gets back its power
	// save, its rfkill state and NetworkManager.
	managed := harness.Managed(net)
	powerSave := "on"
	if m := wlanPowerSaveRE.FindStringSubmatch(harness.Run("iw", "dev", net, "get", "power_save").Stdout); m != nil {
		powerSave = m[1]
	}
	harness.Release(net)
	// A new wiphy starts blocked while the host's WiFi is switched off.
	softs, _ := filepath.Glob(filepath.Join("/sys/class/ieee80211", phy, "rfkill*/soft"))
	blocked := map[string]string{}
	for _, soft := range softs {
		blocked[soft] = harness.Read(soft)
		harness.Write(soft, "0")
	}
	harness.Run("ip", "netns", "del", wlanBenchNS)
	defer func() {
		harness.RunArgv(ns("iw", "dev", net, "set", "power_save", powerSave))
		harness.RunArgv(ns("iw", "phy", phy, "set", "netns", "1"))
		harness.Run("ip", "netns", "del", wlanBenchNS)
		for soft, v := range blocked {
			harness.Write(soft, v)
		}
		if managed {
			harness.Run("nmcli", "device", "set", net, "managed", "yes")
		}
	}()
	for _, argv := range [][]string{
		{"ip", "netns", "add", wlanBenchNS},
		{"iw", "phy", phy, "set", "netns", "name", wlanBenchNS},
		ns("ip", "link", "set", "lo", "up"),
		ns("ip", "link", "set", net, "up"),
		ns("iw", "dev", net, "set", "power_save", "off"),
	} {
		if err := harness.MustRun(argv[0], argv[1:]...); err != nil {
			return err
		}
	}
	start := func(logName string, argv ...string) (*exec.Cmd, error) {
		logf, err := os.Create(filepath.Join(dir, logName))
		if err != nil {
			return nil, err
		}
		defer logf.Close()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return cmd, cmd.Start()
	}
	stop := func(cmd *exec.Cmd) {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
	if !s.noServer {
		argv := bench.Pin(s.opts.PeerCPUs, "iperf3", "-s", "-B", s.server, "-p", fmt.Sprint(wlanBenchPort))
		server, err := start("iperf3-server.log", argv...)
		if err != nil {
			return err
		}
		defer stop(server)
	}

	// runJob runs one repetition of a job with its CPU use and the guest's
	// DMA mappings.
	runJob := func(job string, rep int, run func() (map[string]float64, error)) error {
		if s.ctx.Err() != nil {
			return errors.New("stopped by a signal")
		}
		var dma0 map[string]int64
		if s.guest != nil {
			dma0 = s.guest.DMACounters()
		}
		start := time.Now()
		var cgroup string
		if s.guest != nil {
			cgroup = s.guest.Cgroup
		}
		meter := bench.StartMeter(s.opts.Sets(), cgroup)
		m, err := run()
		cpu := meter.Stop()
		elapsed := time.Since(start)
		if err != nil {
			return fmt.Errorf("%s: %v", job, err)
		}
		for k, v := range cpu {
			m[k] = v
		}
		if s.guest != nil {
			for k, v := range bench.DMARates(dma0, s.guest.DMACounters(), elapsed) {
				m[k] = v
			}
		}
		j := wlanBenchJobs[slices.IndexFunc(wlanBenchJobs, func(j bench.Job) bool { return j.Name == job })]
		fmt.Printf("  %s %d: %s\n", job, rep, bench.Format(j, m))
		s.results.Runs = append(s.results.Runs, bench.Run{Job: job, Target: in.Target, Round: in.Round, Rep: rep, Metrics: m})
		if s.guest != nil {
			s.memory.SampleGuest(s.guest.Proc("meminfo"))
		}
		return nil
	}

	err := runJob("scan", 1, func() (map[string]float64, error) {
		argv := app("iw", "dev", net, "scan")
		t0 := time.Now()
		r := harness.RunContext(s.ctx, dir, time.Minute, argv[0], argv[1:]...)
		elapsed := time.Since(t0).Seconds()
		if !r.OK() {
			return nil, fmt.Errorf("iw scan: %s", strings.TrimSpace(r.Output()))
		}
		return map[string]float64{"seconds": elapsed, "bss": float64(len(wlanBSSRE.FindAllString(r.Stdout, -1)))}, nil
	})
	if err != nil {
		return err
	}

	// The configuration gets a control interface of the test's own, which
	// keeps the passphrase off the command line.
	data, err := os.ReadFile(s.conf)
	if err != nil {
		return err
	}
	ctrl := filepath.Join(dir, "wpa")
	conf := filepath.Join(dir, "wpa_supplicant.conf")
	text := "ctrl_interface=" + ctrl + "\n" + regexp.MustCompile(`(?m)^ctrl_interface=.*\n`).ReplaceAllString(string(data), "")
	if err := os.WriteFile(conf, []byte(text), 0600); err != nil {
		return err
	}
	var wpa *exec.Cmd
	err = runJob("connect", 1, func() (map[string]float64, error) {
		t0 := time.Now()
		var err error
		if wpa, err = start("wpa_supplicant.log", app("wpa_supplicant", "-i", net, "-D", "nl80211", "-c", conf)...); err != nil {
			wpa = nil
			return nil, err
		}
		joined := harness.Wait(func() bool {
			return strings.Contains(harness.RunArgv(app("wpa_cli", "-p", ctrl, "-i", net, "status")).Stdout, "wpa_state=COMPLETED")
		}, 30*time.Second, 20*time.Millisecond)
		if !joined {
			return nil, fmt.Errorf("wpa_supplicant did not join the network, see %s", filepath.Join(dir, "wpa_supplicant.log"))
		}
		return map[string]float64{"seconds": time.Since(t0).Seconds()}, nil
	})
	if wpa != nil {
		defer stop(wpa)
	}
	if err != nil {
		return err
	}
	ps := wlanPowerSaveRE.FindStringSubmatch(harness.RunArgv(ns("iw", "dev", net, "get", "power_save")).Stdout)
	if ps == nil || ps[1] != "off" {
		return fmt.Errorf("power save is not off on %s", net)
	}
	r := harness.RunArgv(ns("busybox", "udhcpc", "-i", net, "-f", "-n", "-q", "-t", "5", "-T", "2",
		"-s", filepath.Join(s.out, "udhcpc.sh")))
	if !r.OK() {
		return fmt.Errorf("no DHCP lease: %s", strings.TrimSpace(r.Output()))
	}
	in.Info = map[string]string{
		"iw link":    strings.TrimSpace(harness.RunArgv(ns("iw", "dev", net, "link")).Stdout),
		"power_save": ps[1],
		"ethtool -i": strings.TrimSpace(harness.RunArgv(ns("ethtool", "-i", net)).Stdout),
	}
	for _, line := range strings.Split(in.Info["iw link"], "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ": "); ok {
			in.Info["link "+k] = v
		}
	}

	ping := func(count int) (map[string]float64, error) {
		argv := app("ping", "-q", "-c", fmt.Sprint(count), "-i", "0.2", "-W", "2", s.server)
		r := harness.RunContext(s.ctx, dir, time.Duration(count)*time.Second+time.Minute, argv[0], argv[1:]...)
		p, ok := harness.ParsePing(r.Output())
		if !ok {
			return nil, fmt.Errorf("ping printed %q", strings.TrimSpace(r.Output()))
		}
		return map[string]float64{"avg_ms": p.Avg, "max_ms": p.Max, "mdev_ms": p.Mdev, "loss_pct": p.Loss}, nil
	}
	iperf := func(ctx context.Context, reverse bool) (map[string]float64, error) {
		argv := app("iperf3", "-c", s.server, "-p", fmt.Sprint(wlanBenchPort), "-t", fmt.Sprint(s.seconds),
			"-O", fmt.Sprint(s.omit), "-J")
		if reverse {
			argv = append(argv, "-R")
		}
		var res harness.Iperf
		var err error
		// The server stops listening for a moment between runs.
		for i := 0; i < 5; i++ {
			r := harness.RunContext(ctx, dir, time.Duration(s.seconds+s.omit+60)*time.Second, argv[0], argv[1:]...)
			if res, err = harness.ParseIperf(r.Stdout); err == nil || !strings.Contains(err.Error(), "refused") {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			return nil, err
		}
		return map[string]float64{"mbps": res.ReceivedBPS / 1e6, "retransmits": float64(res.Retransmits)}, nil
	}
	for rep := 1; rep <= s.opts.Reps; rep++ {
		if err := runJob("ping-idle", rep, func() (map[string]float64, error) { return ping(50) }); err != nil {
			return err
		}
		if err := runJob("upload", rep, func() (map[string]float64, error) { return iperf(s.ctx, false) }); err != nil {
			return err
		}
		if err := runJob("download", rep, func() (map[string]float64, error) { return iperf(s.ctx, true) }); err != nil {
			return err
		}
		// The round trips under load are measured during one more download.
		err := runJob("ping-load", rep, func() (map[string]float64, error) {
			ctx, cancel := context.WithCancel(s.ctx)
			done := make(chan struct{})
			go func() {
				iperf(ctx, true)
				close(done)
			}()
			time.Sleep(time.Duration(s.omit+1) * time.Second)
			m, err := ping(max(1, (s.seconds-2)*5))
			cancel()
			<-done
			return m, err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func cmdBenchWlanSummary(args []string) error {
	fs := newFlags("bench-wlan-summary", `
usage: invirtio-test bench-wlan-summary <output dir>

Print the summary of a bench-wlan run from its results.json.`)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	r, err := bench.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Print(wlanBenchSummary(r))
	return nil
}

// wlanBenchSummary shows the link of each round's connection before the
// summary of the jobs.
func wlanBenchSummary(r *bench.Results) string {
	var b strings.Builder
	for _, in := range r.Instances {
		fmt.Fprintf(&b, "%s round %d: %s MHz, %s, tx %s, rx %s, signal %s\n", in.Target, in.Round,
			in.Info["link freq"], in.Info["link SSID"], in.Info["link tx bitrate"], in.Info["link rx bitrate"],
			in.Info["link signal"])
	}
	b.WriteString("\n")
	return b.String() + r.Summary(wlanBenchJobs)
}
