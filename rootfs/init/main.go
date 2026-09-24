// Command init is HAProxyOS's PID 1. It mounts proc/sysfs/devtmpfs,
// sets up an ephemeral tmpfs writable layer (see mountEphemeral) and the
// persistent STATE partition (see mountState), prints a fixed success
// marker that hack/qemu-run.sh greps for, then:
//   - if /sbin/haproxyosd is present in the initramfs, starts it under a
//     Supervisor (see supervisor.go) that restarts it - with a growing,
//     capped backoff - every time it exits, forever, on an ordinary
//     boot. There's no give-up threshold: haproxyosd is the only way to
//     reach the node at all (see docs/architecture.md's "no shell"
//     design), so a node that stops retrying after N crashes would be
//     permanently unmanageable with no fallback - unlike systemd's
//     default, which can still fall back to SSH. The *one* exception is
//     a boot with a pending LifecycleService.Upgrade(wait_for_health=true)
//     confirmation marker still outstanding (see checkBootCommit and
//     internal/bootcommit): there, restarting forever would just leave
//     an unconfirmed, unhealthy slot unreachable forever too, so
//     Supervisor is bounded and gives up in favor of reverting to the
//     previously-known-good slot instead - a different action from
//     "giving up" on reaching the node at all.
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

	"github.com/swenske/HAProxyOS/internal/bootcommit"
	"github.com/swenske/HAProxyOS/internal/bootslot"
	"github.com/swenske/HAProxyOS/internal/espswitch"
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

	// boot/: internal/bootcommit's pending-confirmation marker for a
	// wait_for_health LifecycleService.Upgrade - needs no first-boot
	// seeding trick like haproxy/ does, since bootcommit.Read treats a
	// missing marker file as simply "nothing pending", not an error.
	bootDir := filepath.Join(stateRoot, "boot")
	if err := os.MkdirAll(bootDir, 0o755); err != nil {
		fmt.Printf("init: mkdir %s: %v\n", bootDir, err)
	} else {
		bindMount(bootDir, bootcommit.Dir)
	}
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
	pendingMarker := checkBootCommit()

	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		release = []byte("unknown")
	}

	fmt.Println("HAProxyOS init")
	fmt.Printf("kernel: %s", release) // osrelease already ends in \n

	if _, err := os.Stat(daemonPath); err == nil {
		startDaemon(pendingMarker)
		return
	}

	fmt.Println("HAPROXYOS_INIT_BOOT_OK")
	// Give the console a moment to flush before the machine goes away.
	time.Sleep(2 * time.Second)
	powerOff()
}

// defaultStableAfter is how long haproxyosd has to keep running before
// a boot with no pending boot-commit marker (the overwhelming majority
// of them) is considered "recovered" for Supervisor's own backoff
// purposes. A pending marker (see checkBootCommit) can override this
// per-boot via its own HealthTimeoutSeconds, sourced from
// UpgradeRequest.health_timeout_seconds.
const defaultStableAfter = 60 * time.Second

func startDaemon(pendingMarker *bootcommit.Marker) {
	if err := os.MkdirAll("/run/haproxyos", 0o755); err != nil {
		fmt.Printf("init: mkdir /run/haproxyos: %v\n", err)
	}

	fmt.Println("HAPROXYOS_INIT_BOOT_OK")

	stableAfter := defaultStableAfter
	if pendingMarker != nil && pendingMarker.HealthTimeoutSeconds > 0 {
		stableAfter = time.Duration(pendingMarker.HealthTimeoutSeconds) * time.Second
	}

	sv := &Supervisor{
		Path:        daemonPath,
		Args:        []string{"-addr", ":9505"},
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		MinBackoff:  1 * time.Second,
		MaxBackoff:  30 * time.Second,
		StableAfter: stableAfter,
		OnStable:    confirmBootCommit,
	}

	// A pending marker means this boot is provisional (see
	// checkBootCommit) - bound how long Supervisor keeps restarting a
	// haproxyosd that's crash-looping without ever confirming healthy,
	// instead of the unconditional "restart forever" every other boot
	// gets. Without this, a slot whose haproxyosd never becomes stable
	// but also never causes a *kernel*-level reboot on its own (a plain
	// userspace crash loop, as opposed to a panic) would sit unreachable
	// forever - checkBootCommit's own cross-boot TriesLeft check only
	// ever gets a chance to act on a boot *after* this one, which
	// requires the machine to actually reboot again first.
	if pendingMarker != nil {
		sv.GiveUpAfter = stableAfter
		sv.OnGiveUp = func() { giveUpBootCommit(pendingMarker) }
	}

	sv.Run()
}

// giveUpBootCommit is Supervisor's OnGiveUp hook, wired up only when
// this boot has a pending marker: haproxyosd has been restarted for
// GiveUpAfter without ever reaching OnStable, so whatever's wrong with
// this slot isn't fixing itself. Re-reads the marker rather than
// trusting the one it closed over, since OnStable could have raced it
// and already cleared it (a legitimately-confirmed boot crashing later,
// for an unrelated reason, must not trigger a revert) - in which case
// this is a deliberate no-op.
func giveUpBootCommit(marker *bootcommit.Marker) {
	cur, err := bootcommit.Read()
	if err != nil || cur == nil || cur.Slot != marker.Slot {
		return
	}
	fmt.Printf("init: haproxyosd never stabilized for slot %s - giving up and reverting to slot %s\n", cur.Slot, cur.RevertTo)
	revertAndReboot(cur)
}

// confirmBootCommit is Supervisor's OnStable hook: once haproxyosd has
// run for the boot's own stableAfter without crashing, whatever
// wait_for_health boot-commit marker was pending for this slot (see
// checkBootCommit) is cleared - this boot is good, stop treating it as
// provisional. A no-op, logging nothing, on the overwhelming majority
// of boots, which have no marker to clear at all.
func confirmBootCommit() {
	marker, err := bootcommit.Read()
	if err != nil {
		fmt.Printf("init: read boot-commit marker for confirmation: %v\n", err)
		return
	}
	if marker == nil {
		return
	}
	if err := bootcommit.Clear(); err != nil {
		fmt.Printf("init: clear boot-commit marker after a stable boot: %v\n", err)
		return
	}
	fmt.Printf("init: boot-commit confirmed for slot %s after a stable boot\n", marker.Slot)
}

// checkBootCommit runs after mountState (needs bootcommit.Dir already
// bind-mounted) and before startDaemon - see internal/bootcommit's own
// package doc for the full mechanism. Three outcomes:
//
//   - no marker, or a marker for some slot other than the one this boot
//     is actually running from (e.g. a Rollback happened in between,
//     making it stale): nothing to do - the marker, if any, is cleared
//     and this returns nil.
//   - a marker for *this* slot with tries remaining: this is the one
//     confirmation attempt LifecycleService.Upgrade granted it. The
//     marker's TriesLeft is decremented and saved *before* handing off
//     to startDaemon - so that if this very boot never confirms
//     (crashes, hangs, gets power-cycled) and the machine comes back up
//     on this same slot again, the *next* call here finds tries already
//     exhausted. Returns the (decremented) marker so startDaemon can
//     honor its HealthTimeoutSeconds.
//   - a marker for this slot with no tries left: a previous boot into
//     this same slot already used its one attempt without ever
//     confirming - whatever's wrong with it isn't fixing itself, so
//     there's no point starting haproxyosd for a third time. Reverts
//     the ESP back to RevertTo (espswitch.Activate - the exact
//     mechanism LifecycleService.Rollback uses, just triggered by init
//     instead of a gRPC call), clears the marker, and reboots
//     immediately - main() never reaches startDaemon at all this boot.
func checkBootCommit() *bootcommit.Marker {
	marker, err := bootcommit.Read()
	if err != nil {
		fmt.Printf("init: read boot-commit marker: %v\n", err)
		return nil
	}
	if marker == nil {
		return nil
	}

	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		fmt.Printf("init: read /proc/cmdline for boot-commit check: %v\n", err)
		return nil
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return nil
	}
	currentSlot, ok := bootslot.ActiveSlot(dataDev)
	if !ok {
		return nil
	}

	if marker.Slot != currentSlot {
		fmt.Printf("init: boot-commit marker is for slot %s, but this boot is slot %s - stale, clearing\n", marker.Slot, currentSlot)
		if err := bootcommit.Clear(); err != nil {
			fmt.Printf("init: clear stale boot-commit marker: %v\n", err)
		}
		return nil
	}

	if marker.TriesLeft > 0 {
		marker.TriesLeft--
		if err := bootcommit.Write(marker); err != nil {
			fmt.Printf("init: write boot-commit marker: %v\n", err)
		}
		fmt.Printf("init: boot-commit pending for slot %s - awaiting confirmation\n", currentSlot)
		return marker
	}

	fmt.Printf("init: boot-commit for slot %s never confirmed after its one attempt - reverting to slot %s\n", currentSlot, marker.RevertTo)
	revertAndReboot(marker)
	// revertAndReboot only returns on failure (its success path reboots
	// or blocks forever) - fall through to a normal boot of this
	// (apparently still not fully healthy) slot rather than leaving the
	// node completely unreachable.
	return nil
}

// revertAndReboot switches the ESP back to marker.RevertTo (the exact
// mechanism LifecycleService.Rollback uses, just triggered locally
// instead of over gRPC), clears the marker, and reboots - the success
// path never returns, since PID 1 must not fall through to starting
// haproxyosd on a slot that just proved itself unhealthy. Called from
// two places: checkBootCommit (a *later* boot finding tries already
// exhausted) and giveUpBootCommit (this *same* boot, once Supervisor's
// GiveUpAfter elapses without ever reaching OnStable) - the two paths
// that can conclude a slot isn't coming up healthy, one crossing a
// reboot to find out and one not needing to.
func revertAndReboot(marker *bootcommit.Marker) {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		fmt.Printf("init: read /proc/cmdline to revert: %v\n", err)
		return
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		fmt.Println("init: couldn't parse /proc/cmdline to revert")
		return
	}
	espDevice, ok := bootslot.ESPDevice(dataDev)
	if !ok {
		fmt.Printf("init: couldn't derive the ESP device from %s, cannot revert\n", dataDev)
		return
	}
	if err := espswitch.Activate(espDevice, marker.RevertTo); err != nil {
		fmt.Printf("init: revert to slot %s failed: %v\n", marker.RevertTo, err)
		if err := bootcommit.Clear(); err != nil {
			fmt.Printf("init: clear boot-commit marker: %v\n", err)
		}
		return
	}
	if err := bootcommit.Clear(); err != nil {
		fmt.Printf("init: clear boot-commit marker: %v\n", err)
	}
	syscall.Sync()

	fmt.Println("init: rebooting to complete the revert")
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		fmt.Printf("init: reboot: %v\n", err)
		return
	}
	// Block here (like powerOff does) until the reboot above actually
	// takes effect, rather than returning and letting a caller fall
	// through to something that assumes this boot is still going.
	select {}
}

func powerOff() {
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Printf("init: reboot(POWER_OFF): %v\n", err)
	}
	// PID 1 must never return - if power-off somehow didn't take effect,
	// hang here instead of letting main() exit (which panics the kernel).
	select {}
}
