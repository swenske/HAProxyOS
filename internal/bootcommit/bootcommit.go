// Package bootcommit implements the persistent "boot pending
// confirmation" marker LifecycleService.Upgrade's wait_for_health uses
// for automatic rollback: Upgrade (internal/api/lifecycle.go) writes a
// marker before rebooting into the newly-upgraded slot, and two
// independent mechanisms watch it from there on, each catching a
// failure mode the other can't:
//
//   - cmd/janusd, once it starts, calls Confirm (below) against a
//     real HAProxy health signal (its own stats socket responding, via
//     internal/haproxy.Manager.ShowInfo) - not just "this process is
//     still running", which says nothing about whether HAProxy itself
//     ever came up. This is the primary, meaningful check.
//   - rootfs/init's own Supervisor.GiveUpAfter/OnGiveUp
//     (rootfs/init/main.go) bounds how long it keeps restarting a
//     janusd that crashes too fast, or too often, to ever reach the
//     point of running its own Confirm loop at all - the one failure
//     mode cmd/janusd can't catch, since it requires janusd to
//     actually be executing. checkBootCommit's own cross-boot
//     TriesLeft tracking is this mechanism's backstop in turn, for a
//     boot too broken (a kernel panic, say) for even Supervisor to run.
//
// Both revert the same way, via internal/bootrevert.To.
//
// Deliberately lives outside internal/api (which pulls in grpc/status)
// so rootfs/init - PID 1, no shell, no reason to link a gRPC stack in
// order to check a JSON file - can use it too. Needs nothing beyond
// encoding/json, os and time.
package bootcommit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Dir is bind-mounted from the persistent STATE partition's own boot/
// subdirectory by rootfs/init/main.go's mountState - the same pattern
// pki/ and haproxy/ already use, so the marker survives exactly the
// reboot it exists to detect a failure across. A var, not a const, so
// tests can point it at a temp directory instead of the real path.
var Dir = "/etc/janus/boot"

const markerFile = "pending.json"

// Marker records an in-progress LifecycleService.Upgrade(wait_for_health=true)
// that hasn't yet been confirmed healthy.
type Marker struct {
	// Slot is the newly-active slot awaiting confirmation - the one
	// this marker is "for". A marker found on a boot into any other
	// slot (e.g. because a Rollback happened in between) is stale.
	Slot string `json:"slot"`
	// RevertTo is the slot to switch back to if Slot never confirms.
	RevertTo string `json:"revert_to"`
	// TriesLeft counts down the number of *additional* boots into Slot
	// rootfs/init will allow before concluding it never will confirm
	// and reverting. Written as 1 by Upgrade: one shot - the very next
	// boot into Slot either confirms (marker cleared) or doesn't, in
	// which case the boot after *that* one - finding TriesLeft already
	// at 0 - triggers the revert without giving Slot yet another try.
	TriesLeft int `json:"tries_left"`
	// HealthTimeoutSeconds, if set, is how long cmd/janusd's own
	// Confirm call (and rootfs/init's GiveUpAfter bound alongside it)
	// waits for this boot to prove healthy before giving up and
	// reverting - the UpgradeRequest.health_timeout_seconds the caller
	// asked for. Zero means "use the caller's own default" (see
	// Confirm's defaultTimeout parameter, and rootfs/init/main.go's
	// defaultGiveUpAfter).
	HealthTimeoutSeconds int `json:"health_timeout_seconds,omitempty"`
}

func path() string {
	return filepath.Join(Dir, markerFile)
}

// Read returns the current marker, or (nil, nil) if none is pending -
// the overwhelmingly common case: every boot that isn't the immediate
// aftermath of a wait_for_health Upgrade.
func Read() (*Marker, error) {
	data, err := os.ReadFile(path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Write persists m, creating Dir if needed - it's a bind-mount target
// that a node upgrading from a build predating this package won't have
// pre-created.
func Write(m *Marker) error {
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path(), data, 0o644)
}

// Clear removes the marker. A no-op, not an error, if none exists -
// every caller (rootfs/init's stale/irrelevant-marker handling, and
// Confirm below) calls this unconditionally rather than checking
// existence first.
func Clear() error {
	err := os.Remove(path())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Confirm polls healthy every pollInterval until either it succeeds
// stableChecks times in a row - real, application-level health, not
// just "the process calling this is still running" - in which case the
// marker is cleared and Confirm returns (true, nil); or m's own
// deadline (HealthTimeoutSeconds, falling back to defaultTimeout if
// zero) elapses first, in which case revert is called instead and
// Confirm returns (false, err) - err is revert's own return value: nil
// if the revert (and whatever reboot it triggers) itself succeeded, in
// which case there's nothing further for the caller to do; non-nil if
// even the revert failed, which the caller should treat as a real
// error to surface loudly, since the node may now be stuck on an
// unconfirmed slot with no automatic recovery left.
//
// Callers pass their own healthy/revert - this package stays free of
// any dependency on what "healthy" or "revert" actually mean for a
// given caller (cmd/janusd wires healthy to a real HAProxy stats-
// socket check and revert to internal/bootrevert.To; tests wire both
// to fakes), keeping this function's own logic - the stability
// counting and deadline arithmetic - unit-testable without a real
// HAProxy process, ESP device, or reboot.
func Confirm(m *Marker, healthy func() error, revert func() error, pollInterval time.Duration, stableChecks int, defaultTimeout time.Duration) (confirmed bool, err error) {
	timeout := defaultTimeout
	if m.HealthTimeoutSeconds > 0 {
		timeout = time.Duration(m.HealthTimeoutSeconds) * time.Second
	}
	deadline := time.Now().Add(timeout)

	consecutive := 0
	for {
		if healthy() == nil {
			consecutive++
			if consecutive >= stableChecks {
				return true, Clear()
			}
		} else {
			consecutive = 0
		}
		if !time.Now().Before(deadline) {
			return false, revert()
		}
		time.Sleep(pollInterval)
	}
}
