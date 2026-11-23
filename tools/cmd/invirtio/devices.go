package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"invirtio/internal/command"
	"invirtio/internal/pci"
	"invirtio/internal/record"
	"invirtio/internal/vdpa"
)

func cmdEnable(args []string) error {
	fs := flag.NewFlagSet("enable", flag.ExitOnError)
	name := fs.String("name", "", "the device's name (default <type>-<address>)")
	memory := fs.String("memory", "", "the guest's memory, such as 768M")
	memoryMax := fs.String("memory-max", "", "the limit of the guest's cgroup, such as 1G (default the guest's memory and 512M)")
	options := fs.String("options", "", "comma-separated options of the backend, such as mtu=9000")
	guestArgs := fs.String("args", "", "space-separated arguments for the guest's command line, such as vfio_uml.eager_dma=1")
	irqCPUs := fs.String("irq-cpus", "", "the host CPUs that the device's interrupts go to, such as 4")
	rom := fs.String("rom", "", "for a GPU, the expansion ROM image that its guest sees, such as the VBIOS of an APU")
	mesa := fs.String("mesa", defaultMesaDir, "for a GPU, the Mesa prefix that graphical sessions take, which scripts/mesa-build.sh builds")
	dry := fs.Bool("n", false, "show the commands of each stage without running them")
	yes := fs.Bool("y", false, "run each stage without waiting for Enter")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio enable [-n] [-y] [-name <name>] [-memory <size>] [-memory-max <size>]\n"+
			"                       [-options <list>] [-args <args>] [-irq-cpus <cpus>] [-rom <file>]\n"+
			"                       [-mesa <prefix>] <pci address>\n\n"+
			"Run the device's driver in a guest until the device is disabled or the host\n"+
			"reboots, and export the device to the host as a virtio device. The host's\n"+
			"own driver lets go of it. A GPU's guest shows the host's display manager on\n"+
			"the GPU's connectors. Each stage shows its commands and waits for Enter\n"+
			"before it runs them.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	if !*dry {
		requireRoot()
	}
	addr, err := pci.NormalizeAddress(fs.Arg(0))
	if err != nil {
		return err
	}
	typ, err := pci.Type(addr)
	if err != nil {
		return err
	}
	kernel, _, modinfo := guestFiles(typ)
	if _, err := os.Stat(kernel); err != nil {
		if typ == "gpu" {
			return errors.New("the guest of GPUs is not installed, run invirtio install with -gpu-guest")
		}
		return errors.New("InvirtIO is not installed, run invirtio install")
	}
	// systemd does not start a transient unit in place of a template's
	// instance.
	if _, err := os.Stat(legacyUnit); err == nil {
		return errors.New("the service of an earlier version is installed, run invirtio install to remove it")
	}
	drv, err := pci.GuestDriver(addr, modinfo)
	if err != nil {
		return err
	}
	mesaSet := false
	fs.Visit(func(f *flag.Flag) { mesaSet = mesaSet || f.Name == "mesa" })
	if typ == "gpu" {
		if _, err := displayManager(); err != nil {
			return err
		}
		if !*dry && graphicalSession() {
			return errors.New("enabling a GPU ends the graphical sessions, so run it from a text console or a remote login")
		}
		if *mesa, err = filepath.Abs(*mesa); err != nil {
			return err
		}
		if err := checkMesa(*mesa); err != nil {
			return err
		}
		if *rom != "" {
			if *rom, err = filepath.Abs(*rom); err != nil {
				return err
			}
			if _, err := os.Stat(*rom); err != nil {
				return err
			}
		}
	} else if *rom != "" || mesaSet {
		return errors.New("-rom and -mesa are only for GPUs")
	}
	if err := pci.CheckGroup(addr); err != nil {
		return err
	}
	dev := &record.Device{Address: addr, Type: typ, Memory: *memory, IRQCPUs: *irqCPUs,
		Args: strings.Fields(*guestArgs)}
	if *memory != "" {
		if _, err := record.ParseSize(*memory); err != nil {
			return err
		}
	}
	if *memoryMax != "" {
		if dev.MemoryMax, err = record.ParseSize(*memoryMax); err != nil {
			return err
		}
	}
	if *options != "" {
		dev.Options = strings.Split(*options, ",")
	}
	if *irqCPUs != "" && !cpuListRE.MatchString(*irqCPUs) {
		return fmt.Errorf("%q is not a CPU list like 4 or 4-5", *irqCPUs)
	}

	devs, err := record.All()
	if err != nil {
		return err
	}
	// Only one process can open a VFIO group, so two devices in one group
	// cannot each have a guest.
	group, _ := pci.IOMMUGroup(addr)
	for n, d := range devs {
		if d.Address == addr {
			return fmt.Errorf("%s is already enabled as %s, disable it first", addr, n)
		}
		if g, err := pci.IOMMUGroup(d.Address); err == nil && g == group {
			return fmt.Errorf("%s is in IOMMU group %s with %s, which is enabled as %s", addr, group, d.Address, n)
		}
	}
	if *name == "" {
		*name = defaultName(typ, addr)
	}
	if err := record.CheckName(*name); err != nil {
		return err
	}
	if _, ok := devs[*name]; ok {
		return fmt.Errorf("a device named %s is already enabled", *name)
	}
	recordActions := []action{{
		show: showCmd("install", "-d", "-m", "0711", record.Dir) + " && " +
			showCmd("install", "-d", "-m", "0755", record.DevicesDir),
		run: record.MakeDirs,
	}}
	if typ == "gpu" {
		// BAR0 is the VRAM aperture of an AMD GPU, which the guest
		// writes, and the guest gives the host's display manager the
		// connectors without fbdev emulation of its own.
		dev.WCBars = "0x1"
		dev.Mesa = *mesa
		if !slices.Contains(dev.Options, "display") {
			dev.Options = append([]string{"display"}, dev.Options...)
		}
		dev.Args = append([]string{"drm_kms_helper.fbdev_emulation=0"}, dev.Args...)
		if *rom != "" {
			dev.ROM = romPath(*name)
			recordActions = append(recordActions, copyAction(*rom, dev.ROM, 0600))
		}
	}
	data, err := record.Encode(dev)
	if err != nil {
		return err
	}
	recordActions = append(recordActions, writeAction(record.Path(*name), string(data), 0644))

	host := pci.Driver(addr)
	if host == "" {
		host = "no driver"
	}
	fmt.Printf("%s is a %s device, which the guest drives with %s. On the host it has %s.\n\n",
		addr, typ, drv, host)
	unit := record.Unit(*name)
	stages := []stage{{title: "record the device as " + *name, actions: recordActions}}
	if typ == "gpu" {
		stages = append(stages, stage{
			title: "switch the display manager to " + *name,
			notes: "This ends the graphical sessions. A transient unit stops the display\n" +
				"manager, starts the guest as " + unit + ", makes the display\n" +
				"manager bind to it and graphical sessions take " + *mesa + ",\n" +
				"and starts the display manager. If it does not start, everything goes\n" +
				"back to the host's driver. Its log: journalctl -u invirtio-display-" + *name,
			actions: []action{displayAction(true, *name)},
		})
	} else {
		stages = append(stages, stage{
			title: "start the guest",
			notes: "The transient service " + unit + " moves the device to vfio-pci,\n" +
				"starts the guest as the " + guestUser + " user, and attaches the guest's device\n" +
				"with vdpa dev add. It lasts until the device is disabled or the host\n" +
				"reboots. Its log: journalctl -u " + unit,
			actions: []action{startAction(*name, typ)},
		})
	}
	if err := runStages(stages, *dry, *yes); err != nil {
		if _, serr := os.Stat(record.Path(*name)); serr == nil {
			log := unit
			if typ == "gpu" {
				log = "invirtio-display-" + *name
			}
			return fmt.Errorf("%v, see journalctl -u %s, and clean up with: invirtio disable %s", err, log, *name)
		}
		return err
	}
	if !*dry {
		fmt.Printf("\nEnabled %s. Check it with: invirtio status %s\n", *name, *name)
	}
	return nil
}

var cpuListRE = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)

// startArgs returns the arguments of systemd-run that start a device's guest
// as a transient service. KillMode=mixed sends the stop signal to the
// supervisor alone, which removes the device and stops the guest in order.
// Delegate lets the supervisor give the guest a cgroup of its own, and
// DelegateSubgroup starts the supervisor in a subgroup beside it. The
// service is ready once the device is on the host, which systemd-run waits
// for. A start can wait for the guest to create its device and for
// processes to let go of a GPU, and a stop can wait for VDUSE's message
// timeout and then for the launcher's. A guest that exits is not restarted,
// and its service stays failed until the device is disabled.
func startArgs(name, typ string) []string {
	stop := "120"
	if typ == "gpu" {
		stop = "240"
	}
	return []string{
		"--unit=" + record.Unit(name), "--description=InvirtIO driver guest for " + name, "--quiet",
		"-p", "Type=notify", "-p", "NotifyAccess=main", "-p", "TimeoutStartSec=240",
		"-p", "TimeoutStopSec=" + stop, "-p", "KillMode=mixed",
		"-p", "Delegate=yes", "-p", "DelegateSubgroup=supervisor",
		binPath, "run", name,
	}
}

func startAction(name, typ string) action {
	return cmdAction("systemd-run", startArgs(name, typ)...)
}

func cmdDisable(args []string) error {
	fs := flag.NewFlagSet("disable", flag.ExitOnError)
	dry := fs.Bool("n", false, "show the commands of each stage without running them")
	yes := fs.Bool("y", false, "run each stage without waiting for Enter")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio disable [-n] [-y] <name | pci address>\n\nStop the device's guest and return the device to its host driver. Each\nstage shows its commands and waits for Enter before it runs them.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	if !*dry {
		requireRoot()
	}
	key := strings.ToLower(fs.Arg(0))
	if addr, err := pci.NormalizeAddress(key); err == nil {
		key = addr
	}
	name, dev, err := record.Find(key)
	if err != nil {
		return err
	}
	if dev.Type == "gpu" && !*dry && graphicalSession() {
		return errors.New("disabling a GPU can end the graphical sessions, so run it from a text console or a remote login")
	}
	if err := runStages(disablePlan(name, dev), *dry, *yes); err != nil {
		return err
	}
	if !*dry {
		drv := pci.Driver(dev.Address)
		if drv == "" {
			drv = "no driver"
		}
		fmt.Printf("\nDisabled %s. %s now has %s.\n", name, dev.Address, drv)
	}
	return nil
}

// disablePlan returns the stages that disable a device. The record goes
// last, so that a disable that stops partway can be run again.
func disablePlan(name string, dev *record.Device) []stage {
	forget := []action{removeAction(record.Path(name))}
	if dev.ROM != "" {
		forget = append(forget, removeAction(dev.ROM))
	}
	return append(disableStages(name, dev), stage{title: "forget the device", actions: forget})
}

// disableStages stops a device's guest, removes what a failed guest left on
// the host, and returns the device to its host driver. A GPU's display
// manager goes back to the host's driver first.
func disableStages(name string, dev *record.Device) []stage {
	if dev.Type != "gpu" {
		return guestStages(name, dev)
	}
	return append([]stage{{
		title: name + ": switch the display manager back to the host's driver",
		notes: "This ends the graphical sessions if the display manager runs on the guest.\n" +
			"A transient unit stops it, stops the guest, returns the GPU to its host\n" +
			"driver and starts it again. Its log: journalctl -u invirtio-display-" + name,
		actions: []action{displayAction(false, name)},
	}}, guestStages(name, dev)...)
}

// guestStages stops a device's guest, removes what a failed guest left on
// the host, and returns the device to its host driver.
func guestStages(name string, dev *record.Device) []stage {
	unit := record.Unit(name)
	vduse := "/sys/class/vduse/" + name
	addr := dev.Address
	// A device that is gone from the PCI bus has no driver to return to.
	present := func() bool {
		_, err := os.Stat(pci.Path(addr, ""))
		return err == nil
	}
	onVFIO := func() bool { return pci.Driver(addr) == "vfio-pci" }
	unbound := func() bool { return present() && pci.Driver(addr) == "" }
	return []stage{
		{
			title: name + ": stop the guest",
			notes: "On stop, the service removes the device from the host before it stops\n" +
				"the guest. A failed service stays until reset-failed. Both steps are\n" +
				"skipped for a service that systemd does not know.",
			actions: []action{unitAction(unit, "stop"), unitAction(unit, "reset-failed")},
		},
		{
			title: name + ": remove what a failed guest left on the host",
			notes: "A step is skipped when there is nothing for it to remove.",
			actions: []action{
				sysfsAction(vduse+"/msg_timeout", "1", func() bool { return vdpa.Attached(name) }),
				{
					show: showCmd("vdpa", "dev", "del", name),
					run: func() error {
						return vdpa.Remove(name, true)
					},
				},
				{
					show: "# the VDUSE_DESTROY_DEV ioctl for " + name + " on /dev/vduse/control, which no command makes",
					run:  func() error { return vdpa.Destroy(name) },
				},
				removeAction(logPath(name)),
				{
					show: showCmd("umount", logDir(name)),
					// The stopped guest can leave the log's fd in a cycle
					// of Unix sockets, which the kernel frees soon after,
					// so a busy tmpfs is tried for a second.
					run: func() error {
						for n := 0; isMountPoint(logDir(name)); n++ {
							err := unix.Unmount(logDir(name), 0)
							if err == nil || err != unix.EBUSY || n == 100 {
								return err
							}
							time.Sleep(10 * time.Millisecond)
						}
						return nil
					},
				},
				{
					show: showCmd("rmdir", logDir(name)),
					run: func() error {
						if err := os.Remove(logDir(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
							return err
						}
						return nil
					},
				},
			},
		},
		{
			title: name + ": return " + addr + " to its host driver",
			notes: "The unbind is skipped unless vfio-pci has the device, and the probe\nunless no driver has it. All three are skipped for a device that is gone\nfrom the PCI bus.",
			actions: []action{
				sysfsAction(pci.Path(addr, "driver_override"), "", present),
				sysfsAction(pci.Path(addr, "driver/unbind"), addr, onVFIO),
				sysfsAction("/sys/bus/pci/drivers_probe", addr, unbound),
			},
		},
	}
}

// unitAction runs a systemctl verb on a unit, unless systemd knows nothing
// of it. A failed unit whose file is gone stays known until reset-failed.
func unitAction(unit, verb string) action {
	return action{
		show: showCmd("systemctl", verb, unit),
		run: func() error {
			if systemctlShow(unit, "LoadState") == "not-found" && systemctlShow(unit, "ActiveState") == "inactive" {
				return nil
			}
			return command.Run("systemctl", verb, unit)
		},
	}
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	dry := fs.Bool("n", false, "show the commands of each stage without running them")
	yes := fs.Bool("y", false, "run each stage without waiting for Enter")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio uninstall [-n] [-y]\n\nDisable every device, returning it to its host driver, and remove what\ninstall added. Each stage shows its commands and waits for Enter before it\nruns them.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	if !*dry {
		requireRoot()
	}
	devs, err := record.All()
	if err != nil {
		return err
	}
	for _, d := range devs {
		if d.Type == "gpu" && !*dry && graphicalSession() {
			return errors.New("uninstalling with a GPU enabled can end the graphical sessions, so run it from a text console or a remote login")
		}
	}

	if err := checkLegacyDevices(); err != nil {
		return err
	}
	var stages []stage
	for _, name := range record.Names(devs) {
		stages = append(stages, disableStages(name, devs[name])...)
	}
	stages = append(stages, stage{title: "remove what earlier versions installed", actions: legacyActions()})
	// The user goes before the files, and this program last, so that a
	// failed step can be run again with the installed program.
	var remove []action
	if _, err := user.Lookup(guestUser); err == nil {
		remove = append(remove, cmdAction("userdel", guestUser))
	}
	remove = append(remove, cmdAction("rm", "-rf", libDir, runDir), cmdAction("rm", "-f", binPath))
	stages = append(stages, stage{title: "remove InvirtIO", actions: remove})
	if err := runStages(stages, *dry, *yes); err != nil {
		return err
	}
	if !*dry {
		fmt.Println("\nUninstalled InvirtIO.")
	}
	return nil
}

// checkLegacyDevices fails if the legacy device list names a device, whose
// driver and display manager only the installed program that wrote the list
// can restore.
func checkLegacyDevices() error {
	data, err := os.ReadFile(legacyConfig)
	if err != nil {
		return nil
	}
	var cfg struct {
		Devices map[string]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("%s: %v", legacyConfig, err)
	}
	if len(cfg.Devices) == 0 {
		return nil
	}
	names := make([]string, 0, len(cfg.Devices))
	for n := range cfg.Devices {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("%s lists %s, so disable each first with the installed %s disable <name>, "+
		"which also removes a GPU's ROM image from %s", legacyConfig, strings.Join(names, ", "), binPath, legacyConfigDir)
}

// legacyActions remove the legacy service, udev rule and device list where
// they are present. The rest of /etc/invirtio stays.
func legacyActions() []action {
	var acts []action
	links, _ := filepath.Glob("/etc/systemd/system/*.wants/invirtio@*.service")
	for _, l := range links {
		acts = append(acts, removeAction(l))
	}
	if _, err := os.Stat(legacyUnit); err == nil || len(links) > 0 {
		acts = append(acts, removeAction(legacyUnit), cmdAction("systemctl", "reset-failed", "invirtio@*.service"))
	}
	acts = append(acts, cmdAction("systemctl", "daemon-reload"))
	if _, err := os.Stat(legacyUdevRule); err == nil {
		acts = append(acts, removeAction(legacyUdevRule), cmdAction("udevadm", "control", "--reload"))
	}
	if _, err := os.Stat(legacyConfig); err == nil {
		acts = append(acts, removeAction(legacyConfig), action{
			show: showCmd("rmdir", "--ignore-fail-on-non-empty", legacyConfigDir),
			run: func() error {
				err := os.Remove(legacyConfigDir)
				if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
					return err
				}
				return nil
			},
		})
	}
	return acts
}

// removeAction removes a file, unless it is gone.
func removeAction(path string) action {
	return action{
		show: showCmd("rm", "-f", path),
		run: func() error {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		},
	}
}
