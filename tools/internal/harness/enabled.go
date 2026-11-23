package harness

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"invirtio/internal/record"
	"invirtio/internal/vdpa"
)

// Enabled is the running guest of a device that invirtio enabled.
type Enabled struct {
	Name   string
	Device *record.Device
	// Cgroup is the guest's cgroup, below its service's.
	Cgroup string
}

// FindEnabled returns the guest of the enabled device at a PCI address,
// once its service runs and its device is on the vDPA bus.
func FindEnabled(addr string) (*Enabled, error) {
	name, dev, err := record.Find(addr)
	if errors.Is(err, record.ErrNotEnabled) {
		return nil, fmt.Errorf("%s is not enabled, so enable it first with: invirtio enable %s", addr, addr)
	}
	if err != nil {
		return nil, err
	}
	unit := record.Unit(name)
	if state := strings.TrimSpace(Run("systemctl", "is-active", unit).Stdout); state != "active" {
		return nil, fmt.Errorf("the service of %s is %s, see journalctl -u %s", name, state, unit)
	}
	if !vdpa.Attached(name) {
		return nil, fmt.Errorf("%s is not on the vDPA bus", name)
	}
	cg := record.GuestCgroup(name)
	if cg == "" {
		return nil, fmt.Errorf("the guest of %s has no cgroup of its own", name)
	}
	return &Enabled{Name: name, Device: dev, Cgroup: cg}, nil
}

// CheckDeviceCPU checks that the guest runs on cpu, as a benchmark's
// -device-cpu says, with the device's interrupts there too. An empty cpu
// asks for nothing.
func (e *Enabled) CheckDeviceCPU(cpu string) error {
	if cpu == "" || (e.Device.IRQCPUs == cpu && slices.Contains(e.Device.Args, "cpu_pin="+cpu)) {
		return nil
	}
	return fmt.Errorf("%s is not pinned to the device CPU %s, so disable it and enable it again with -irq-cpus %s and cpu_pin=%s in -args",
		e.Name, cpu, cpu, cpu)
}

// Proc reads a file of the guest's /proc through its management console,
// or returns "".
func (e *Enabled) Proc(file string) string {
	text, _ := record.Proc(e.Name, file)
	return strings.TrimLeft(text, "\n")
}

// Counters returns the guest's VDUSE counters.
func (e *Enabled) Counters() map[string]int64 {
	return ParseCounters(e.Proc("vduse_guest"))
}

// DMACounters returns the guest's VFIO DMA counters.
func (e *Enabled) DMACounters() map[string]int64 {
	return ParseSection(e.Proc("vfio_uml"), "dma")
}

// SaveIRQs returns a function that sets the CPUs that VDUSE injects each
// queue's interrupts from back to what they are now.
func SaveIRQs(name string) func() {
	files, _ := filepath.Glob(filepath.Join("/sys/class/vduse", name, "vq*/irq_cb_affinity"))
	saved := map[string]string{}
	for _, f := range files {
		saved[f] = Read(f)
	}
	return func() {
		for f, mask := range saved {
			Write(f, mask)
		}
	}
}

// Describe adds the guest's name, record and kernel to a benchmark's
// environment.
func (e *Enabled) Describe(env map[string]string) {
	env["guest_name"] = e.Name
	env["guest_version"] = strings.TrimSpace(e.Proc("version"))
	env["guest_cmdline"] = strings.TrimSpace(e.Proc("cmdline"))
	if data, err := record.Encode(e.Device); err == nil {
		env["guest_record"] = string(data)
	}
}
