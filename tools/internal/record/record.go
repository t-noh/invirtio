// Package record holds the records of the devices that invirtio enabled,
// which last until they are disabled or the host reboots.
package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"invirtio/internal/mconsole"
	"invirtio/internal/pci"
)

// Dir holds each enabled device's record, sockets and in-flight log. Others
// can pass through it but not list it, and each file in it has its own
// mode, so that any user can read the records in DevicesDir while the
// sockets and logs stay root's.
const (
	Dir        = "/run/invirtio"
	DevicesDir = Dir + "/devices"
)

// Device is one host device that a guest drives and exports.
type Device struct {
	Address string `json:"address"`
	// Type is the backend that exports the device: wlan, net or gpu.
	Type string `json:"type"`
	// Memory is the guest's memory, as UML's mem= takes it.
	Memory string `json:"memory,omitempty"`
	// MemoryMax bounds all the memory of the guest's cgroup, in bytes.
	MemoryMax int64 `json:"memory_max,omitempty"`
	// Options are added to the guest's vduse_guest.device= argument.
	Options []string `json:"options,omitempty"`
	// Args are added to the guest's command line.
	Args []string `json:"args,omitempty"`
	// IRQCPUs, if set, are the host CPUs that the device's interrupts go to.
	IRQCPUs string `json:"irq_cpus,omitempty"`
	// ROM, if set, is the expansion ROM image that the guest sees.
	ROM string `json:"rom,omitempty"`
	// WCBars, if set, is the mask of the prefetchable BARs that vfio-pci
	// maps write-combined.
	WCBars string `json:"wc_bars,omitempty"`
	// Mesa is the Mesa prefix that graphical sessions take while a GPU's
	// guest shows the display manager.
	Mesa string `json:"mesa,omitempty"`
}

var defaultMemory = map[string]string{
	"wlan": "256M",
	"net":  "256M",
	"gpu":  "2G",
}

// GuestMemory returns the guest's mem=.
func (d *Device) GuestMemory() string {
	if d.Memory != "" {
		return d.Memory
	}
	return defaultMemory[d.Type]
}

// CgroupLimit returns the limit of the guest's cgroup, by default the
// guest's memory with room for UML's host-side allocations and the pages it
// pins for DMA.
func (d *Device) CgroupLimit() (int64, error) {
	if d.MemoryMax > 0 {
		return d.MemoryMax, nil
	}
	mem, err := ParseSize(d.GuestMemory())
	if err != nil {
		return 0, err
	}
	return mem + 512<<20, nil
}

// MemlockMiB returns the guest's RLIMIT_MEMLOCK in MiB, which holds all of
// its RAM and room for the buffers that it maps beyond it, and at least
// 1024 MiB.
func (d *Device) MemlockMiB() int64 {
	mem, err := ParseSize(d.GuestMemory())
	if err != nil {
		return 1024
	}
	return max(1024, (mem+(1<<20)-1)>>20+256)
}

// ParseSize parses a size as UML's mem= does, with an optional K, M or G.
func ParseSize(s string) (int64, error) {
	shift := 0
	num := s
	if s != "" {
		switch strings.ToUpper(s[len(s)-1:]) {
		case "K":
			shift = 10
		case "M":
			shift = 20
		case "G":
			shift = 30
		}
		if shift != 0 {
			num = s[:len(s)-1]
		}
	}
	var n int64
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil || n <= 0 ||
		fmt.Sprint(n) != num || n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("cannot parse the size %q", s)
	}
	return n << shift, nil
}

// A name is a VDUSE device name and a systemd instance name, and it appears
// in paths and on the guest's command line.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// CheckName checks that a name can name a device.
func CheckName(name string) error {
	// The VDUSE control device is called control, the console lines of a
	// device called invirtio would look like the supervisor's messages, and
	// the log directory of a device called devices would be DevicesDir.
	if !nameRE.MatchString(name) || name == "control" || name == "invirtio" || name == "devices" {
		return fmt.Errorf("%q is not a valid device name, which has up to 32 lowercase letters, digits and dashes", name)
	}
	return nil
}

func Path(name string) string         { return filepath.Join(DevicesDir, name+".json") }
func MconsolePath(name string) string { return filepath.Join(Dir, name+".mconsole") }

// Unit is the transient systemd service that runs a device's guest.
func Unit(name string) string { return "invirtio@" + name + ".service" }

// ErrNotEnabled is the error of Load and Find for a device that is not
// enabled.
var ErrNotEnabled = errors.New("not enabled")

// Load reads a device's record.
func Load(name string) (*Device, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(Path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no device %s is enabled: %w", name, ErrNotEnabled)
	}
	if err != nil {
		return nil, err
	}
	d := &Device{}
	if err := json.Unmarshal(data, d); err != nil {
		return nil, fmt.Errorf("%s: %v", Path(name), err)
	}
	if _, ok := defaultMemory[d.Type]; !ok {
		return nil, fmt.Errorf("%s: the unknown type %q", Path(name), d.Type)
	}
	if !pci.ValidAddress(d.Address) {
		return nil, fmt.Errorf("%s: the invalid address %q", Path(name), d.Address)
	}
	return d, nil
}

// All reads the records of every enabled device.
func All() (map[string]*Device, error) {
	files, _ := filepath.Glob(filepath.Join(DevicesDir, "*.json"))
	devs := map[string]*Device{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		d, err := Load(name)
		if err != nil {
			return nil, err
		}
		devs[name] = d
	}
	return devs, nil
}

// Names returns the names of devices in order.
func Names(devs map[string]*Device) []string {
	names := make([]string, 0, len(devs))
	for name := range devs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Find finds an enabled device by its name or its PCI address.
func Find(key string) (string, *Device, error) {
	devs, err := All()
	if err != nil {
		return "", nil, err
	}
	if d, ok := devs[key]; ok {
		return key, d, nil
	}
	for name, d := range devs {
		if d.Address == key {
			return name, d, nil
		}
	}
	return "", nil, fmt.Errorf("no device %s is enabled: %w", key, ErrNotEnabled)
}

// Encode returns a record as it is stored.
func Encode(d *Device) ([]byte, error) {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// MakeDirs makes Dir and DevicesDir with their modes.
func MakeDirs() error {
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{Dir, 0711}, {DevicesDir, 0755}} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	return nil
}

// GuestCgroup returns the cgroup that the supervisor gave the guest of a
// running service, or "".
func GuestCgroup(name string) string {
	out, err := exec.Command("systemctl", "show", "-p", "ControlGroup", "--value", Unit(name)).Output()
	rel := strings.TrimSpace(string(out))
	if err != nil || rel == "" {
		return ""
	}
	dir := filepath.Join("/sys/fs/cgroup", rel, "guest")
	if _, err := os.Stat(filepath.Join(dir, "memory.current")); err != nil {
		return ""
	}
	return dir
}

// Proc reads a file of the guest's /proc through its management console.
func Proc(name, file string) (string, error) {
	return mconsole.Request(MconsolePath(name), "proc "+file)
}
