// Package espswitch mounts the ESP and makes a given A/B slot's
// already-staged Unified Kernel Image the one \EFI\BOOT\BOOTX64.EFI
// actually boots - a plain file copy, no PE manipulation, no build
// tooling, ever, on the node: the target OS has no package manager and
// can never shell out to ukify/sbsign itself, which is exactly why
// image/disk/activate-slot.sh stages *both* slots' UKIs on the ESP
// (\HAPROXYOS\UKI-A.EFI / UKI-B.EFI) at build/install time in the first
// place.
//
// Shared by two independent processes that both need this same
// mechanism, triggered two different ways: internal/api/lifecycle.go's
// LifecycleService.Rollback (an explicit, gRPC-driven slot switch) and
// rootfs/init's own automatic revert when a wait_for_health Upgrade
// never confirms healthy (see internal/bootcommit) - factored out once
// there were two real consumers, not duplicated a second time across
// that package boundary, the same reasoning internal/bootslot was
// factored out for.
package espswitch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Mountpoint is under /run, which rootfs/init/main.go's mountEphemeral
// already makes a fresh tmpfs on every boot - nothing pre-existing
// there needs preserving.
const Mountpoint = "/run/haproxyos/esp"

// ErrUKINotStaged distinguishes "the target slot's UKI was never
// staged there" (image/disk/activate-slot.sh never ran for this disk -
// a setup problem, not a runtime one) from any other failure. Wrapped
// with %w so callers can match it with errors.Is regardless of what
// else the error text says.
var ErrUKINotStaged = errors.New("staged UKI not found on the ESP")

// Mount mounts espDevice (vfat) at Mountpoint, creating it first.
func Mount(espDevice string) error {
	if err := os.MkdirAll(Mountpoint, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", Mountpoint, err)
	}
	if err := syscall.Mount(espDevice, Mountpoint, "vfat", 0, ""); err != nil {
		return fmt.Errorf("mount ESP %s: %w", espDevice, err)
	}
	return nil
}

// Unmount unmounts Mountpoint.
func Unmount() error {
	return syscall.Unmount(Mountpoint, 0)
}

// Activate mounts espDevice, copies slot's pre-staged UKI
// (\HAPROXYOS\UKI-<slot>.EFI) over \EFI\BOOT\BOOTX64.EFI, syncs, and
// unmounts - the full sequence for switching to a slot whose UKI is
// already sitting on the ESP (as opposed to LifecycleService.Upgrade's
// own ESP write, which stages a *new* UKI from a release bundle first -
// that case uses Mount/Unmount directly instead of this helper). Best-
// effort unmount: a failure to unmount is not this function's failure
// to report, matching how a mount left behind doesn't affect whether
// the UKI was actually switched.
func Activate(espDevice, slot string) error {
	if err := Mount(espDevice); err != nil {
		return err
	}
	defer func() { _ = Unmount() }()

	src := filepath.Join(Mountpoint, "HAPROXYOS", fmt.Sprintf("UKI-%s.EFI", slot))
	staged, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: slot %s (%s): %v", ErrUKINotStaged, slot, src, err)
		}
		return fmt.Errorf("read staged UKI for slot %s (%s): %w", slot, src, err)
	}

	dst := filepath.Join(Mountpoint, "EFI", "BOOT", "BOOTX64.EFI")
	if err := os.WriteFile(dst, staged, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	syscall.Sync()
	return nil
}
