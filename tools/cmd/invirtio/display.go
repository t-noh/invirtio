package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"invirtio/internal/command"
	"invirtio/internal/pci"
	"invirtio/internal/record"
	"invirtio/internal/sysfs"
	"invirtio/internal/vdpa"
)

// cmdDisplay switches the host's display manager to a GPU's guest and back.
// Enable and disable run it as a transient unit, since stopping the display
// manager ends the graphical sessions and whatever runs in them.
func cmdDisplay(args []string) error {
	fs := flag.NewFlagSet("display", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio display on|off <name>\n\nSwitch the display manager to the guest of a GPU, or back to the GPU's\nhost driver. Enable and disable run this.")
	}
	fs.Parse(args)
	if fs.NArg() != 2 || (fs.Arg(0) != "on" && fs.Arg(0) != "off") {
		fs.Usage()
		os.Exit(2)
	}
	requireRoot()
	name := fs.Arg(1)
	dev, err := record.Load(name)
	if err != nil {
		return err
	}
	if dev.Type != "gpu" {
		return fmt.Errorf("%s is not a GPU", name)
	}
	dm, err := displayManager()
	if err != nil {
		return err
	}
	if fs.Arg(0) == "on" {
		return displayOn(name, dev, dm)
	}
	return displayOff(name, dev, dm)
}

// displayOn stops the display manager, starts the guest, makes the display
// manager bind to the guest's service and graphical sessions take the
// device's Mesa, and starts the display manager again. If the display
// manager does not start, everything goes back to the GPU's host driver.
func displayOn(name string, dev *record.Device, dm string) error {
	logf("stopping %s", dm)
	if err := command.Run("systemctl", "stop", dm); err != nil {
		return err
	}
	if bind := fbconBind(); bind != "" {
		sysfs.Write(bind, "0\n")
	}
	logf("starting the guest of %s", name)
	if err := command.Run("systemd-run", startArgs(name, dev.Type)...); err != nil {
		return displayBack(name, dev, dm, err)
	}
	if err := writeDisplayFiles(name, dev, dm); err != nil {
		return displayBack(name, dev, dm, err)
	}
	logf("starting %s on %s", dm, name)
	if err := command.Run("systemctl", "start", dm); err != nil {
		return displayBack(name, dev, dm, err)
	}
	return nil
}

// displayOff switches a display manager that runs on the guest back to the
// GPU's host driver. A display manager that was not switched is left alone.
func displayOff(name string, dev *record.Device, dm string) error {
	if _, err := os.Stat(dmDropinPath(dm, name)); err != nil &&
		systemctlState("is-active", record.Unit(name)) != "active" {
		logf("%s does not run on %s", dm, name)
		return nil
	}
	logf("stopping %s", dm)
	if err := command.Run("systemctl", "stop", dm); err != nil {
		return err
	}
	return displayBack(name, dev, dm, nil)
}

// displayBack stops the guest, removes what switched the display manager,
// returns the GPU to its host driver and starts the display manager there.
// It returns cause, or the first error of its own.
func displayBack(name string, dev *record.Device, dm string, cause error) error {
	if cause != nil {
		warn("switching %s back to the host's driver: %v", dm, cause)
	}
	keep := func(err error) {
		if err != nil && cause == nil {
			cause = err
		}
	}
	keep(unitAction(record.Unit(name), "stop").run())
	// A guest that exited by itself leaves its device on the bus, which
	// goes once the display manager and its clients have let go of it.
	if vdpa.Exists(name) {
		cleanup(name, true)
	}
	keep(removeDisplayFiles(name, dm))
	keep(returnToHost(dev.Address))
	if bind := fbconBind(); bind != "" {
		keep(sysfs.Write(bind, "1\n"))
	}
	logf("starting %s on the host's driver", dm)
	keep(command.Run("systemctl", "start", dm))
	return cause
}

// writeDisplayFiles writes the files that switch the display manager to the
// guest. The display manager binds to the guest's service, and graphical
// sessions take the device's Mesa.
func writeDisplayFiles(name string, dev *record.Device, dm string) error {
	unit := record.Unit(name)
	files := map[string]string{
		runEnvPath: "LD_LIBRARY_PATH=" + dev.Mesa + "/lib\n" +
			"VK_DRIVER_FILES=" + dev.Mesa + "/share/vulkan/icd.d/radeon_icd.x86_64.json\n" +
			"__EGL_VENDOR_LIBRARY_FILENAMES=" + dev.Mesa + "/share/glvnd/egl_vendor.d/50_mesa.json\n" +
			"GBM_BACKENDS_PATH=" + dev.Mesa + "/lib/gbm\n",
		dmDropinPath(dm, name): "[Unit]\nBindsTo=" + unit + "\nAfter=" + unit + "\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := writeAction(path, content, 0644).run(); err != nil {
			return err
		}
	}
	return reloadManagers()
}

func removeDisplayFiles(name, dm string) error {
	for _, path := range []string{runEnvPath, dmDropinPath(dm, name)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return reloadManagers()
}

// reloadManagers reloads systemd and the user managers, which read
// environment.d when they start or reload. A user manager outlives its last
// session for a while, and one that a remote login keeps can live on.
func reloadManagers() error {
	if err := command.Run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	out, err := exec.Command("loginctl", "list-users", "--no-legend").Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			if err := command.Run("systemctl", "--user", "-M", f[1]+"@", "daemon-reload"); err != nil {
				warn("cannot reload the user manager of %s: %v", f[1], err)
			}
		}
	}
	return nil
}

// returnToHost gives a device back to its host driver.
func returnToHost(addr string) error {
	if _, err := os.Stat(pci.Path(addr, "")); err != nil {
		return nil
	}
	if err := sysfs.Write(pci.Path(addr, "driver_override"), "\n"); err != nil {
		return err
	}
	if pci.Driver(addr) == "vfio-pci" {
		if err := sysfs.Write(pci.Path(addr, "driver/unbind"), addr); err != nil {
			return err
		}
	}
	if pci.Driver(addr) == "" {
		return sysfs.Write("/sys/bus/pci/drivers_probe", addr)
	}
	return nil
}

// displayManager returns the unit of the host's display manager.
func displayManager() (string, error) {
	out, err := exec.Command("systemctl", "show", "-p", "Id,LoadState", "display-manager.service").Output()
	if err != nil {
		return "", fmt.Errorf("cannot find the display manager: %v", err)
	}
	var id, state string
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "Id="); ok {
			id = v
		} else if v, ok := strings.CutPrefix(line, "LoadState="); ok {
			state = v
		}
	}
	if state != "loaded" || id == "" || id == "display-manager.service" {
		return "", errors.New("the host has no display manager behind display-manager.service")
	}
	return id, nil
}

// fbconBind returns the bind file of the framebuffer console, or "".
func fbconBind() string {
	dirs, _ := filepath.Glob("/sys/class/vtconsole/vtcon*")
	for _, d := range dirs {
		if name, err := os.ReadFile(filepath.Join(d, "name")); err == nil &&
			strings.Contains(string(name), "frame buffer") {
			return filepath.Join(d, "bind")
		}
	}
	return ""
}

// checkMesa checks that a prefix holds the Mesa that scripts/mesa-build.sh
// installs, and that only root can change it, since the display manager's
// greeter and every user's session load it.
func checkMesa(prefix string) error {
	libs, _ := filepath.Glob(filepath.Join(prefix, "lib", "libgallium-*.so"))
	if len(libs) == 0 {
		return fmt.Errorf("%s holds no Mesa, build it with scripts/mesa-build.sh %s", prefix, prefix)
	}
	for _, p := range []string{prefix, filepath.Join(prefix, "lib"), libs[0]} {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if owner(st) != 0 || st.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("%s can be changed by users other than root, which the display manager's greeter and every session would load", p)
		}
	}
	return nil
}

// graphicalSession reports whether this process runs in a graphical session,
// which stopping the display manager ends. A logind session of a text
// console or a remote login is not one, even with a forwarded display. In
// any other session, such as the user manager's own, where the programs that
// it starts run, one of the ancestors having a display counts.
func graphicalSession() bool {
	if id, err := os.ReadFile("/proc/self/sessionid"); err == nil {
		out, err := exec.Command("loginctl", "show-session", strings.TrimSpace(string(id)), "-p", "Type", "--value").Output()
		switch strings.TrimSpace(string(out)) {
		case "wayland", "x11", "mir":
			return err == nil
		case "tty":
			if err == nil {
				return false
			}
		}
	}
	for pid := os.Getppid(); pid > 1; {
		env, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		for _, v := range strings.Split(string(env), "\x00") {
			if strings.HasPrefix(v, "WAYLAND_DISPLAY=") || strings.HasPrefix(v, "DISPLAY=") {
				return true
			}
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return false
		}
		// The parent follows the command, which is in parentheses.
		f := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
		if len(f) < 2 {
			return false
		}
		pid, _ = strconv.Atoi(f[1])
	}
	return false
}

// owner returns a file's owner.
func owner(st os.FileInfo) uint32 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Uid
	}
	return ^uint32(0)
}

func dmDropinPath(dm, name string) string {
	return filepath.Join(runUnitDir, dm+".d", "50-invirtio-"+name+".conf")
}

// displaySwitched reports whether a display manager binds to a device's
// guest.
func displaySwitched(name string) bool {
	m, _ := filepath.Glob(filepath.Join(runUnitDir, "*.d", "50-invirtio-"+name+".conf"))
	return len(m) > 0
}

// displayAction runs invirtio display in a transient unit and waits for it,
// so that it finishes when the session that started it ends.
func displayAction(on bool, name string) action {
	mode := "off"
	if on {
		mode = "on"
	}
	return cmdAction("systemd-run", "--wait", "--collect", "--quiet", "--unit=invirtio-display-"+name,
		binPath, "display", mode, name)
}
