package main

import (
	"path/filepath"
	"strings"

	"invirtio/internal/record"
)

// Where install puts InvirtIO.
const (
	libDir    = "/usr/lib/invirtio"
	binPath   = "/usr/local/sbin/invirtio"
	guestUser = "invirtio"
	runDir    = record.Dir

	// Each device's log is on a tmpfs of its own, since the guest can write
	// to its log without bound, and a full /run would hold up the host's
	// services. The size is just above the largest log that the launcher
	// lets the guest lay out.
	logTmpfsOptions = "size=33m,nr_inodes=8,mode=0700"
)

// The legacy service, udev rule and device list started guests at boot, and
// install and uninstall remove them.
const (
	legacyConfigDir = "/etc/invirtio"
	legacyConfig    = legacyConfigDir + "/devices.json"
	legacyUnit      = "/etc/systemd/system/invirtio@.service"
	legacyUdevRule  = "/etc/udev/rules.d/70-invirtio.rules"
)

var (
	guestPath    = filepath.Join(libDir, "linux")
	initrdPath   = filepath.Join(libDir, "initramfs.cpio")
	launcherPath = filepath.Join(libDir, "invirtio-launch")
	modinfoPath  = filepath.Join(libDir, "modules.builtin.modinfo")

	// The guest of a GPU is a guest of its own, with the GPU's driver and
	// firmware.
	gpuGuestPath   = filepath.Join(libDir, "linux-gpu")
	gpuInitrdPath  = filepath.Join(libDir, "initramfs-gpu.cpio")
	gpuModinfoPath = filepath.Join(libDir, "modules-gpu.builtin.modinfo")
)

// A GPU's guest shows the host's display manager on the GPU's connectors, so
// the files that switch the display manager over are under /run. The display
// manager binds to the guest's service, and graphical sessions take the Mesa
// that renders through the guest from environment.d.
const (
	runUnitDir     = "/run/systemd/system"
	runEnvPath     = "/run/environment.d/50-invirtio-mesa.conf"
	defaultMesaDir = "/opt/invirtio/mesa"
)

// guestFiles returns the guest kernel, initramfs and list of built-in modules
// of a device's type.
func guestFiles(typ string) (kernel, initrd, modinfo string) {
	if typ == "gpu" {
		return gpuGuestPath, gpuInitrdPath, gpuModinfoPath
	}
	return guestPath, initrdPath, modinfoPath
}

// defaultName names a device after its type and address, so that two ports
// of one card get different names.
func defaultName(typ, addr string) string {
	a := strings.TrimPrefix(addr, "0000:")
	a = strings.NewReplacer(":", "-", ".", "-").Replace(a)
	return typ + "-" + a
}

func logDir(name string) string      { return filepath.Join(runDir, name) }
func logPath(name string) string     { return filepath.Join(logDir(name), "inflight") }
func consolePath(name string) string { return filepath.Join(runDir, name+".console") }
func romPath(name string) string     { return filepath.Join(runDir, name+".rom") }
