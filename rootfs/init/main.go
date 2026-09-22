// Command init is HAProxyOS's PID 1 for Phase 1: it proves the boot chain
// (kernel -> initramfs -> a shell-less, statically-linked Go binary as
// init) works under QEMU. It mounts proc/sysfs/devtmpfs, prints a fixed
// success marker that hack/qemu-run.sh greps for, and powers the machine
// off cleanly so the test harness doesn't need a hard timeout.
//
// This is deliberately not the real init (rootfs/init/README.md describes
// what that needs to become in Phase 2: supervising haproxyosd and its
// managed services) - it's the smallest thing that proves the concept.
package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

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

	fmt.Println("HAProxyOS init (Phase 1 boot-proof placeholder)")
	fmt.Printf("kernel: %s", release) // osrelease already ends in \n
	fmt.Println("HAPROXYOS_INIT_BOOT_OK")

	// Give the console a moment to flush before the machine goes away.
	time.Sleep(2 * time.Second)

	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Printf("init: reboot(POWER_OFF): %v\n", err)
	}

	// PID 1 must never return - if power-off somehow didn't take effect,
	// hang here instead of letting main() exit (which panics the kernel).
	select {}
}
