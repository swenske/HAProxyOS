// Command init is HAProxyOS's PID 1. It mounts proc/sysfs/devtmpfs,
// sets up an ephemeral tmpfs writable layer (see mountEphemeral) and the
// persistent STATE partition (see mountState), prints a fixed success
// marker that hack/qemu-run.sh greps for, then:
//   - if /sbin/haproxyosd is present in the initramfs, starts it under a
//     Supervisor (see supervisor.go) that restarts it - with a growing,
//     capped backoff - every time it exits, forever. There's no give-up
//     threshold: haproxyosd is the only way to reach the node at all
//     (see docs/architecture.md's "no shell" design), so a node that
//     stops retrying after N crashes would be permanently unmanageable
//     with no fallback - unlike systemd's default, which can still fall
//     back to SSH.
//   - otherwise, powers off cleanly after a short delay - the Phase 1
//     boot-proof shape, so `make qemu-boot-test` (init alone, no
//     haproxyosd packaged in) keeps working unchanged.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/swenske/HAProxyOS/internal/bootslot"
)

const daemonPath = "/sbin/haproxyosd"

func mount(source, target, fstype string) {
	if err := os.MkdirAll(target, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", target, err)
		return
	}
	if err := syscall.Mount(source, target, fstype, 0, ""); err != nil {
		fmt.Printf("init: mount %s on %s: %v\n", fstype, target, err)
	}
}

// mountEphemeral gives the otherwise fully read-only (Phase 3: dm-verity
// verified) root a writable layer, entirely tmpfs-backed - nothing here
// survives a reboot (see mountState for the one directory that does).
// /run and /tmp are mounted empty - nothing pre-existing there needs to
// survive the overmount (haproxyosd's /run/haproxyos, HAProxy's stats
// socket/pid file, Manager.Validate's tmpfile). /etc needs its own
// handling since it isn't empty on a freshly-booted node: rootfs/base/
// etc/haproxy/haproxy.cfg (the bootstrap default config) lives on the
// squashfs and would otherwise vanish under a plain tmpfs mount, so its
// bytes are read *before* the overmount and rewritten into the new
// tmpfs - this is what makes both PKI bootstrap (/etc/haproxyos/pki,
// see mountState) and a live ApplyConfig RPC (which writes to this same
// path) actually work on a dm-verity-booted node; before this,
// haproxyosd crash-looped forever on "read-only file system" trying to
// create either. /var is left alone (still squashfs-backed): the only
// thing under it is /var/empty, HAProxy's chroot jail, which must keep
// the exact immutable mode-0000 baked into the image by rootfs/
// assemble.sh, not a fresh writable one recreated here.
func mountEphemeral() {
	mount("tmpfs", "/run", "tmpfs")
	mount("tmpfs", "/tmp", "tmpfs")

	const seedCfg = "/etc/haproxy/haproxy.cfg"
	cfgBytes, readErr := os.ReadFile(seedCfg)
	cfgMode := os.FileMode(0o644)
	if info, err := os.Stat(seedCfg); err == nil {
		cfgMode = info.Mode().Perm()
	}

	mount("tmpfs", "/etc", "tmpfs")

	if readErr != nil {
		fmt.Printf("init: read %s before /etc overlay: %v\n", seedCfg, readErr)
		return
	}
	if err := os.MkdirAll(filepath.Dir(seedCfg), 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", filepath.Dir(seedCfg), err)
		return
	}
	if err := os.WriteFile(seedCfg, cfgBytes, cfgMode); err != nil {
		fmt.Printf("init: write %s: %v\n", seedCfg, err)
	}
}

// mountState mounts the pre-formatted, persistent STATE partition (see
// rootfs/state-image.sh) once at /mnt/state, then bind-mounts its pki/
// and haproxy/ subdirectories over /etc/haproxyos/pki and /etc/haproxy
// respectively - the two things mountEphemeral's tmpfs overlay can't be
// allowed to wipe every boot: PKI (or a fresh CA/admin cert generates
// every boot) and applied config (or a live ApplyConfig RPC - which
// just writes straight to /etc/haproxy/haproxy.cfg, see
// internal/haproxy.Manager.Apply - is lost the moment the node
// restarts). Unlike the root device, this one is writable, not
// dm-verity-protected, since it's meant to be written to and
// losing/corrupting it only costs state, not the system's integrity.
//
// Where to find it depends on which of two shapes actually booted -
// resolveStateDevice tells them apart by parsing /proc/cmdline's own
// dm-mod.create= parameter (see internal/bootslot, shared with
// internal/api/lifecycle.go's LifecycleService.Rollback), not a
// separate flag or convention of its own:
//   - the real, single GPT disk (image/disk/assemble.sh, booted by
//     hack/qemu-ab-boot-test.sh): root's data device is a partition
//     (e.g. /dev/vda2), and STATE is always partition 6 on that same
//     disk, by image/disk/assemble.sh's own fixed layout.
//   - the older separate-virtio-blk-drives harness
//     (hack/qemu-verity-boot-test.sh, hack/qemu-state-persist-test.sh),
//     which deliberately keeps working unchanged since it still covers
//     things the single-disk test doesn't (dm-verity tamper detection,
//     STATE persistence in isolation): root's data device is a whole
//     disk (e.g. /dev/vda, no partition table at all), and STATE is a
//     separate, fixed /dev/vdc drive.
//
// Not present at all in Phase 1/2's initramfs boots, or a verity boot
// without the drive mountState resolves to actually attached - mount()
// fails harmlessly there (see its own doc comment), leaving both
// directories on the ephemeral tmpfs instead, same fallback behavior
// as before this existed.
func mountState() {
	// Has to live inside the already-writable tmpfs /etc (mountEphemeral
	// runs first, see main()), not some fresh top-level path like
	// /mnt/state: root itself is still the dm-verity-verified, read-only
	// squashfs, so os.MkdirAll on a path that doesn't already exist
	// there fails outright - caught by a real boot regenerating a new
	// CA every time despite this function running, because every step
	// past that failed MkdirAll silently no-op'd (mount()/bindMount()
	// only log and return on error, never abort the boot).
	const stateRoot = "/etc/.state"
	mount(resolveStateDevice(), stateRoot, "ext4")

	// pki/: created with 0700 directly (matching
	// internal/pki.LoadOrBootstrap's own intent) rather than mounting
	// the STATE device straight at /etc/haproxyos/pki and chmod-ing
	// afterward - a bind mount's target shows the *source* directory's
	// mode, so controlling it here is enough, no separate chmod needed.
	pkiDir := filepath.Join(stateRoot, "pki")
	if err := os.MkdirAll(pkiDir, 0o700); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", pkiDir, err)
	} else {
		bindMount(pkiDir, "/etc/haproxyos/pki")
	}

	// haproxy/: needs the same first-boot seeding problem mountEphemeral
	// already solved for /etc itself - a blank STATE partition's
	// haproxy/ subdirectory has no haproxy.cfg yet, and bind-mounting it
	// over /etc/haproxy as-is would hide the bootstrap default
	// mountEphemeral just put there, leaving HAProxy nothing to start
	// from. So the bootstrap bytes are copied in *only if this is still
	// empty* - once a real ApplyConfig has written something there, a
	// later boot must never overwrite it back to the bootstrap default.
	haproxyDir := filepath.Join(stateRoot, "haproxy")
	seedPersistentHaproxyCfg(haproxyDir)
	bindMount(haproxyDir, "/etc/haproxy")
}

func seedPersistentHaproxyCfg(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", dir, err)
		return
	}

	const cfgName = "haproxy.cfg"
	dst := filepath.Join(dir, cfgName)
	if _, err := os.Stat(dst); err == nil {
		return // already has a persisted config (bootstrap or applied) - never overwrite it
	}

	// Still the mountEphemeral-seeded bootstrap default at this point -
	// mountState always runs after mountEphemeral (see main()).
	src := "/etc/haproxy/" + cfgName
	cfgBytes, err := os.ReadFile(src)
	if err != nil {
		fmt.Printf("init: read %s to seed persistent state: %v\n", src, err)
		return
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(src); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(dst, cfgBytes, mode); err != nil {
		fmt.Printf("init: write %s: %v\n", dst, err)
	}
}

// resolveStateDevice figures out where the STATE partition/drive
// actually is for *this* boot - see mountState's doc comment for the
// two shapes it distinguishes between. Never fails outright: any
// read/parse problem just falls back to the older separate-drive
// convention, same as if this function didn't exist at all.
func resolveStateDevice() string {
	const fallback = "/dev/vdc" // hack/qemu-verity-boot-test.sh / hack/qemu-state-persist-test.sh's separate-drives harness
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		fmt.Printf("init: read /proc/cmdline: %v\n", err)
		return fallback
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return fallback
	}
	device, ok := bootslot.StateDevice(dataDev)
	if !ok {
		return fallback
	}
	return device
}

func bindMount(src, dst string) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", dst, err)
		return
	}
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
		fmt.Printf("init: bind mount %s on %s: %v\n", src, dst, err)
	}
}

func main() {
	mount("proc", "/proc", "proc")
	mount("sysfs", "/sys", "sysfs")
	mount("devtmpfs", "/dev", "devtmpfs")
	mountEphemeral()
	mountState()

	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		release = []byte("unknown")
	}

	fmt.Println("HAProxyOS init")
	fmt.Printf("kernel: %s", release) // osrelease already ends in \n

	if _, err := os.Stat(daemonPath); err == nil {
		startDaemon()
		return
	}

	fmt.Println("HAPROXYOS_INIT_BOOT_OK")
	// Give the console a moment to flush before the machine goes away.
	time.Sleep(2 * time.Second)
	powerOff()
}

func startDaemon() {
	if err := os.MkdirAll("/run/haproxyos", 0o755); err != nil {
		fmt.Printf("init: mkdir /run/haproxyos: %v\n", err)
	}

	fmt.Println("HAPROXYOS_INIT_BOOT_OK")

	(&Supervisor{
		Path:        daemonPath,
		Args:        []string{"-addr", ":9505"},
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		MinBackoff:  1 * time.Second,
		MaxBackoff:  30 * time.Second,
		StableAfter: 60 * time.Second,
	}).Run()
}

func powerOff() {
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Printf("init: reboot(POWER_OFF): %v\n", err)
	}
	// PID 1 must never return - if power-off somehow didn't take effect,
	// hang here instead of letting main() exit (which panics the kernel).
	select {}
}
