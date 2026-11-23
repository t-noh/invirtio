# invirtio

`invirtio` installs InvirtIO and manages its driver guests. Each enabled
device gets a guest of its own, which runs the device's real driver under
the sandboxed launcher and exports the device to the host as a virtio-wlan
or virtio-net device.

All guests run one UML build that has every supported driver and both
backends. `invirtio` picks the backend from the device's PCI class, and the
guest's kernel binds the driver that matches the device.

## Building

```
make -C launcher
make -C tools
scripts/invirtio-build-guest.sh
```

The host must run the `linux-host` kernel, with the IOMMU on and the
`vduse`, `vdpa`, `virtio_vdpa` and `virtio_wlan` modules (see
`linux-host/README.md`), and it needs the `vdpa` command from iproute2.

## Use

```
sudo build/invirtio install -guest linux-uml/build-invirtio
invirtio list
sudo invirtio enable 06:00.0
invirtio status
sudo invirtio console wlan-06-00-0
sudo invirtio disable wlan-06-00-0
sudo invirtio uninstall
```

`install`, `enable`, `disable` and `uninstall` run in stages. Each stage
prints the shell commands that do the same as its steps, and waits for
Enter before it runs them. With `-n`, they print every stage and change
nothing, which needs no root, and with `-y`, they do not wait.

- `install` copies the guest, its initramfs, the launcher and `invirtio`
  to `/usr/lib/invirtio` and `/usr/local/sbin`, and adds the `invirtio`
  user that the guests run as. It also removes the service, udev rule and
  `/etc/invirtio/devices.json` of earlier versions, and refuses to run
  while that list names a device, which the installed `invirtio disable`
  of the earlier version must disable first.
- `enable` checks that the guest has a driver for the device, records it
  in `/run/invirtio/devices/<name>.json`, and starts the transient service
  `invirtio@<name>`. The name defaults to the type and the address, such
  as `wlan-06-00-0`. The device stays in its guest until it is disabled or
  the host reboots, and nothing starts at boot. The guest's settings are
  options of `enable`: `-memory` (the guest's `mem=`), `-memory-max` (the
  guest cgroup's limit), `-options` for the backend's
  `vduse_guest.device=` argument, `-args` for the guest's command line,
  and `-irq-cpus` for the launcher's `-a`. Changing them means disabling
  the device and enabling it again.
- `disable` stops the guest, removes what a failed guest left on the host,
  gives the device back to its host driver, and removes the device's
  record. It also cleans up after an `enable` that failed.
- `list` shows the host's WiFi and Ethernet devices, with the host driver
  that has each now, the guest driver that would drive it, and the name it
  is enabled as.
- `status` shows each device's service and guest settings, its host and
  guest drivers, its VDUSE and vDPA state and its host interface. With
  `-v`, it adds the guest's memory in detail, and with `-vv`, also its
  VDUSE counters.
- `console` attaches to the guest's console, with the last 64 KiB of its
  output, and Ctrl-] detaches. Escape sequences that carry strings, C1
  controls and invalid UTF-8 are dropped from the guest's output, which
  assumes a terminal in UTF-8, as the console sets it when it attaches.

## The service

`enable` starts `invirtio@<name>` with `systemd-run`, which waits until the
device is on the host, so `enable` fails if the guest does not get that
far. The service runs `invirtio run <name>`, which:

1. loads the host modules, and removes a wlan device that an earlier guest
   left on the vDPA bus, since a new guest cannot take one over;
2. gives the guest a cgroup of its own below the service's, with a memory
   limit;
3. runs the launcher on a pty, as the `invirtio` user, with the device and
   its in-flight log, which is on a small tmpfs of its own at
   `/run/invirtio/<name>`, since the guest can write to it without bound;
4. attaches the device with `vdpa dev add` once the guest has created it;
5. serves the console on `/run/invirtio/<name>.console`, which only root
   can open, and writes the guest's console to the journal, filtered as
   the console is. The lines marked `<name> launcher:` hold the launcher's
   messages and the guest's stderr, which the guest shares with it;
6. on stop, removes the device first, while the guest can answer the
   host's reset, then stops the guest.

When a guest exits by itself, a wlan device is removed, and a net device
stays on the bus, where a guest that `systemctl start invirtio@<name>`
starts again reconnects to it. systemd does not restart the service by
itself, and it stays failed until the device is disabled or started again,
so a guest that crashes during a benchmark is not hidden. `status` shows a
net device left on the bus with no guest, and `disable` removes it. The
host's next message to such a device waits for VDUSE's 30 s timeout, after
which VDUSE treats the device as broken until it is removed.
