// Package bootrevert performs the actual mechanics of switching back to
// a previous A/B slot when a pending LifecycleService.Upgrade
// (internal/bootcommit) never confirms healthy: resolving the current
// boot's ESP device from /proc/cmdline, switching it via
// internal/espswitch.Activate, and clearing the marker.
//
// Shared by two independent processes, reaching the same conclusion two
// different ways: rootfs/init's own boot-time revert path
// (checkBootCommit/giveUpBootCommit - for a haproxyosd that crashes too
// fast, or too often, to ever run its own health check at all) and
// cmd/haproxyosd's real HAProxy-level health confirmation
// (internal/bootcommit.Confirm's revert callback - for a haproxyosd
// that runs perfectly fine as a *process* but whose HAProxy never
// actually becomes healthy). Neither process can catch the other's
// failure mode: rootfs/init has no visibility into HAProxy's own
// health, and haproxyosd can't act at all if it never gets to run in
// the first place.
//
// To deliberately stops short of rebooting - callers differ on what
// "after" should look like (rootfs/init blocks forever waiting for its
// own syscall.Reboot to take effect, since PID 1 must never return;
// haproxyosd just issues one and lets the whole machine go down with
// it, no special handling needed) - so this leaves that one step, and
// the syscall.Sync() that should precede it, to the caller.
package bootrevert

import (
	"fmt"
	"os"

	"github.com/swenske/HAProxyOS/internal/bootcommit"
	"github.com/swenske/HAProxyOS/internal/bootslot"
	"github.com/swenske/HAProxyOS/internal/espswitch"
)

// To switches the currently-booted disk's ESP to marker.RevertTo and
// clears marker.
func To(marker *bootcommit.Marker) error {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return fmt.Errorf("read /proc/cmdline: %w", err)
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return fmt.Errorf("couldn't parse a dm-mod.create= data device out of /proc/cmdline")
	}
	espDevice, ok := bootslot.ESPDevice(dataDev)
	if !ok {
		return fmt.Errorf("couldn't derive the ESP device from %s", dataDev)
	}
	if err := espswitch.Activate(espDevice, marker.RevertTo); err != nil {
		return fmt.Errorf("activate slot %s: %w", marker.RevertTo, err)
	}
	if err := bootcommit.Clear(); err != nil {
		return fmt.Errorf("clear boot-commit marker: %w", err)
	}
	return nil
}
