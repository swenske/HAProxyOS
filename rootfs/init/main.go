// Command init is HAProxyOS's PID 1. It mounts proc/sysfs/devtmpfs,
// prints a fixed success marker that hack/qemu-run.sh greps for, then:
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

func main() {
	mount("proc", "/proc", "proc")
	mount("sysfs", "/sys", "sysfs")
	mount("devtmpfs", "/dev", "devtmpfs")

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
