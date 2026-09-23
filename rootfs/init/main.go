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
// rootfs/state-image.sh) over /etc/haproxyos/pki, the one thing from
// mountEphemeral's ephemeral overlay that actually needs to survive a
// reboot - without it, a fresh CA/admin cert gets generated on every
// boot. Expected as the third virtio-blk drive (after the squashfs data
// and dm-verity hash tree drives - see hack/qemu-verity-boot-test.sh vs
// hack/qemu-state-persist-test.sh for the difference), unlike those two
// this one is writable, not dm-verity-protected: it's meant to be
// written to, and losing/corrupting it only costs PKI state, not the
// system's integrity. Not present in Phase 1/2's initramfs boots, or a
// verity boot without a third drive attached - mount() fails harmlessly
// there (see its own doc comment), leaving /etc/haproxyos/pki on the
// ephemeral tmpfs instead, same fallback behavior as before this
// existed. The explicit chmod matches internal/pki.LoadOrBootstrap's
// own intent (0700, not whatever mode mkfs.ext4 gives a fresh
// filesystem's root directory).
func mountState() {
	const dir = "/etc/haproxyos/pki"
	mount("/dev/vdc", dir, "ext4")
	if err := os.Chmod(dir, 0o700); err != nil {
		fmt.Printf("init: chmod %s: %v\n", dir, err)
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
