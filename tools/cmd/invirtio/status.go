package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"invirtio/internal/pci"
	"invirtio/internal/record"
	"invirtio/internal/sysfs"
	"invirtio/internal/vdpa"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	verbose := fs.Bool("v", false, "also show the guest's memory in detail, which needs root for the guest's own view")
	counters := fs.Bool("vv", false, "also show what -v shows and the guest's VDUSE counters, which needs root")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio status [-v | -vv] [<name>]\n\nShow the state of the enabled devices, or of one.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() > 1 {
		fs.Usage()
		os.Exit(2)
	}

	if _, err := os.Stat(guestPath); err != nil {
		fmt.Println("InvirtIO is not installed.")
	}
	devs, err := record.All()
	if err != nil {
		return err
	}
	names := record.Names(devs)
	if fs.NArg() == 1 {
		if _, ok := devs[fs.Arg(0)]; !ok {
			return fmt.Errorf("no device %s is enabled", fs.Arg(0))
		}
		names = []string{fs.Arg(0)}
	}
	if len(names) == 0 {
		fmt.Println("No devices are enabled.")
		return nil
	}
	for i, name := range names {
		if i > 0 {
			fmt.Println()
		}
		level := 0
		if *counters {
			level = 2
		} else if *verbose {
			level = 1
		}
		showDevice(name, devs[name], level)
	}
	return nil
}

// systemctlShow returns a property of a unit.
func systemctlShow(unit, prop string) string {
	out, _ := exec.Command("systemctl", "show", "-p", prop, "--value", unit).Output()
	return strings.TrimSpace(string(out))
}

func systemctlState(verb, unit string) string {
	out, _ := exec.Command("systemctl", verb, unit).Output()
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return "unknown"
}

// showDevice shows a device's state, and with a level of 1 the guest's
// memory in detail, and with 2 its VDUSE counters too.
func showDevice(name string, dev *record.Device, level int) {
	unit := record.Unit(name)
	row := func(label, value string) {
		fmt.Printf("  %-16s %s\n", label+":", value)
	}

	fmt.Printf("%s (%s, %s)\n", name, dev.Type, dev.Address)
	active := systemctlState("is-active", unit)
	row("service", active)
	settings := "mem=" + dev.GuestMemory()
	if dev.IRQCPUs != "" {
		settings += ", interrupts on CPUs " + dev.IRQCPUs
	}
	row("guest", settings)
	if len(dev.Options) > 0 {
		row("backend options", strings.Join(dev.Options, ","))
	}
	if len(dev.Args) > 0 {
		row("guest arguments", strings.Join(dev.Args, " "))
	}
	host := pci.Driver(dev.Address)
	if host == "" {
		host = "none"
	}
	row("host driver", host)
	_, _, modinfo := guestFiles(dev.Type)
	if drv, err := pci.GuestDriver(dev.Address, modinfo); err == nil {
		row("guest driver", drv)
	} else {
		row("guest driver", "none")
	}

	state := "not created"
	if vdpa.Exists(name) {
		state = "created"
		if vdpa.Attached(name) && active != "active" && active != "activating" {
			// A net device stays on the bus for the next guest after its
			// guest exits.
			state += ", on the vDPA bus with no guest, invirtio disable removes it"
		} else if vdpa.Attached(name) {
			state += ", on the vDPA bus"
		} else {
			state += ", not on the vDPA bus"
		}
	}
	row("VDUSE device", state)

	nets := vdpa.HostInterfaces(name)
	if len(nets) == 0 {
		row("host interface", "none")
	}
	for _, n := range nets {
		oper, err := sysfs.Read(filepath.Join("/sys/class/net", n, "operstate"))
		if err != nil {
			oper = "unknown"
		}
		row("host interface", fmt.Sprintf("%s (%s)", n, oper))
	}

	cg := record.GuestCgroup(name)
	if cg == "" {
		row("memory", "no guest runs")
	} else {
		row("memory", memoryUse(cg))
	}

	if level < 1 {
		return
	}
	if cg != "" {
		row("memory detail", memoryDetail(cg))
		if pinned, err := pinnedMemory(cg); err == nil {
			row("pinned for DMA", formatSize(pinned)+" of that memory")
		} else {
			row("pinned for DMA", "not available")
		}
	}
	// The guest writes its memory figures, its counters and any error, so
	// only printable text of theirs reaches the terminal.
	if meminfo, err := record.Proc(name, "meminfo"); err == nil {
		row("guest memory", printable(guestMemory(meminfo)))
	}
	if level < 2 {
		return
	}
	text, err := record.Proc(name, "vduse_guest")
	if err != nil {
		row("guest counters", printable(fmt.Sprintf("not available: %v", err)))
		return
	}
	fmt.Println("  guest counters:")
	for _, line := range strings.Split(strings.TrimRight(printable(text), "\n"), "\n") {
		fmt.Println("    " + line)
	}
}

// printable returns s without invalid UTF-8 and without control characters
// other than tabs and newlines, the C1 controls included.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError, r < 0x20 && r != '\t' && r != '\n', r >= 0x7f && r <= 0x9f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
