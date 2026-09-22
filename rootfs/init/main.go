// Command init is HAProxyOS's PID 1. It mounts proc/sysfs/devtmpfs,
// prints a fixed success marker that hack/qemu-run.sh greps for, then:
//   - if /sbin/haproxyosd is present in the initramfs, starts it as a
//     supervised child (the real Phase 2 shape: init -> haproxyosd ->
//     haproxy) and reaps zombies forever - the machine stays up.
//   - otherwise, powers off cleanly after a short delay - the Phase 1
//     boot-proof shape, so `make qemu-boot-test` (init alone, no
//     haproxyosd packaged in) keeps working unchanged.
package main

import (
	"fmt"
	"os"
	"os/exec"
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

// startDaemon launches haproxyosd (which starts/supervises haproxy - see
// internal/haproxy.Manager) and never returns: PID 1 spends the rest of
// its life reaping zombies. There's no restart-on-crash logic yet - a
// crashed haproxyosd just leaves the machine without a control plane
// until Phase 3's supervision story lands.
func startDaemon() {
	if err := os.MkdirAll("/run/haproxyos", 0o755); err != nil {
		fmt.Printf("init: mkdir /run/haproxyos: %v\n", err)
	}

	cmd := exec.Command(daemonPath, "-addr", ":9505")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Printf("init: start %s: %v\n", daemonPath, err)
		fmt.Println("HAPROXYOS_INIT_BOOT_OK")
		time.Sleep(2 * time.Second)
		powerOff()
		return
	}

	fmt.Println("HAPROXYOS_INIT_BOOT_OK")
	reapForever()
}

// reapForever waits for any child (haproxyosd, or anything it or haproxy
// leaves behind) to exit, forever - the minimum a process running as
// PID 1 owes the kernel, or exited children pile up as zombies.
func reapForever() {
	for {
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(-1, &ws, 0, nil); err != nil {
			time.Sleep(time.Second)
		}
	}
}

func powerOff() {
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Printf("init: reboot(POWER_OFF): %v\n", err)
	}
	// PID 1 must never return - if power-off somehow didn't take effect,
	// hang here instead of letting main() exit (which panics the kernel).
	select {}
}
