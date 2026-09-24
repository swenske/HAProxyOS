// Package bootcommit implements the persistent "boot pending
// confirmation" marker LifecycleService.Upgrade's wait_for_health uses
// for automatic rollback: Upgrade (internal/api/lifecycle.go) writes a
// marker before rebooting into the newly-upgraded slot, rootfs/init
// reads it on every boot to tell whether the slot it's booting into is
// still unconfirmed, and clears it once that boot has run stably for
// long enough (rootfs/init hooks this into Supervisor.OnStable, reusing
// the same "survived this long without crashing" signal Supervisor
// already tracks for its own backoff reset).
//
// Deliberately lives outside internal/api (which pulls in grpc/status)
// so rootfs/init - PID 1, no shell, no reason to link a gRPC stack in
// order to check a JSON file - can use it too. Needs nothing beyond
// encoding/json and os.
//
// What "healthy" means here is deliberately limited: the control-plane
// daemon (haproxyosd) started and kept running for the confirmation
// window, nothing more - there's no HAProxy-level health check (e.g.
// against its own stats socket) feeding into this yet. Promising more
// than that would be dishonest about what's actually verified; a
// process that starts but hangs without ever crashing would still get
// confirmed. Real per-service health checking is a separate, future
// piece.
package bootcommit

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Dir is bind-mounted from the persistent STATE partition's own boot/
// subdirectory by rootfs/init/main.go's mountState - the same pattern
// pki/ and haproxy/ already use, so the marker survives exactly the
// reboot it exists to detect a failure across. A var, not a const, so
// tests can point it at a temp directory instead of the real path.
var Dir = "/etc/haproxyos/boot"

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
	// HealthTimeoutSeconds, if set, overrides how long rootfs/init
	// waits for this boot to prove stable before confirming - the
	// UpgradeRequest.health_timeout_seconds the caller asked for. Zero
	// means "use rootfs/init's own default" (see startDaemon).
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
// every caller (rootfs/init's stability confirmation, and its own
// stale/irrelevant-marker handling) calls this unconditionally rather
// than checking existence first.
func Clear() error {
	err := os.Remove(path())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
