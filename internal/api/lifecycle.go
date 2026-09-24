package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
	"github.com/swenske/HAProxyOS/internal/bootcommit"
	"github.com/swenske/HAProxyOS/internal/bootslot"
	"github.com/swenske/HAProxyOS/internal/espswitch"
)

// Lifecycle implements haproxyosv1alpha1.LifecycleServiceServer.
//
// Rollback and Upgrade share a core trick: the running node can never
// shell out to `ukify`/`sbsign` (no package manager, by design), so both
// just move already-built UKIs into place instead of ever assembling
// one. Rollback moves a UKI image/disk/activate-slot.sh already staged at
// build/install time (\HAPROXYOS\UKI-A.EFI / UKI-B.EFI); Upgrade moves one
// that arrived as part of a "release bundle" (image/release/assemble.sh) -
// which exists specifically because a genuinely *new* rootfs has a root
// hash nobody could have pre-staged at the original install's build time.
// Install (install.go) is the one that can't get away with just moving
// bytes into place throughout: it has no existing partition table to
// build on, so it lays out the whole disk itself in pure Go
// (internal/diskimage + github.com/diskfs/go-diskfs, since sgdisk/
// mtools/mkfs.ext4 don't exist on the target OS either) - but even it
// never builds a UKI, taking both slots' pre-built ones from the same
// kind of release bundle Upgrade reads from.
type Lifecycle struct {
	haproxyosv1alpha1.UnimplementedLifecycleServiceServer
}

// Fixed partition sizes, matching image/disk/assemble.sh's own
// DATA_MB/HASH_MB - Upgrade enforces the same limits that script does
// at build time, rather than letting a write silently run past the end
// of a partition and corrupt whatever comes after it (the next
// partition, per image/disk/assemble.sh's fixed layout).
const (
	upgradeMaxDataBytes = 64 * 1024 * 1024
	upgradeMaxHashBytes = 4 * 1024 * 1024
)

// bootContext is what both Rollback and Upgrade need to know about the
// disk they're running from: which slot is active, which slot they
// should target instead, and where the ESP is. Resolved once from
// /proc/cmdline via internal/bootslot - see mountState in
// rootfs/init/main.go for the same parsing applied to finding STATE
// instead.
type bootContext struct {
	disk        string
	currentSlot string
	targetSlot  string
	espDevice   string
}

func resolveBootContext() (*bootContext, error) {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read /proc/cmdline: %v", err)
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "couldn't find a dm-mod.create= verity data device in /proc/cmdline - not booted from a recognized A/B image")
	}
	current, ok := bootslot.ActiveSlot(dataDev)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "root device %q isn't a recognized A/B slot (see image/disk/assemble.sh's fixed partition layout)", dataDev)
	}
	disk, ok := bootslot.Disk(dataDev)
	if !ok {
		return nil, status.Errorf(codes.Internal, "couldn't derive the disk from root device %q", dataDev)
	}
	espDev, ok := bootslot.ESPDevice(dataDev)
	if !ok {
		return nil, status.Errorf(codes.Internal, "couldn't derive the ESP device from root device %q", dataDev)
	}
	return &bootContext{
		disk:        disk,
		currentSlot: current,
		targetSlot:  bootslot.OtherSlot(current),
		espDevice:   espDev,
	}, nil
}

// scheduleReboot reboots shortly after the caller returns, not before -
// an RPC that rebooted immediately would kill the connection before its
// own response (or, for Upgrade, its last stream message) ever reached
// the caller.
func scheduleReboot() {
	go func() {
		time.Sleep(2 * time.Second)
		syscall.Sync()
		if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
			log.Printf("lifecycle: reboot: %v", err)
		}
	}()
}

// writePartitionFile writes data to a partition device path directly
// (e.g. "/dev/vda4") - the kernel already knows that device's offset
// on the underlying disk from the GPT table it parsed at boot
// (CONFIG_EFI_PARTITION), so this needs no offset math of its own, unlike
// image/disk/assemble.sh's build-time `dd seek=` (which operates on a
// plain disk image file with no partition-aware block devices at all).
func writePartitionFile(device string, data []byte) error {
	f, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func (l *Lifecycle) Rollback(_ context.Context, _ *emptypb.Empty) (*haproxyosv1alpha1.RollbackResponse, error) {
	bc, err := resolveBootContext()
	if err != nil {
		return nil, err
	}

	if err := espswitch.Activate(bc.espDevice, bc.targetSlot); err != nil {
		if errors.Is(err, espswitch.ErrUKINotStaged) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v - was image/disk/activate-slot.sh ever run for this disk?", err)
		}
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	scheduleReboot()

	return &haproxyosv1alpha1.RollbackResponse{ActiveSlot: bc.targetSlot}, nil
}

// Upgrade writes a new rootfs to the currently-inactive A/B slot from a
// local "release bundle" directory (image/release/assemble.sh) -
// req.Source.Reference is that directory's path for now. Real OCI/HTTPS
// distribution (what the proto comment and ImageSource's own field docs
// describe) isn't implemented - this project has no image registry or
// release-signing key infrastructure yet, and building either is a
// distinct, separate concern from the actual upgrade mechanics this
// proves: writing new content into the inactive slot without disturbing
// STATE or the currently-running slot, then switching and rebooting into
// it, the same way LifecycleService.Rollback does for a slot switch with
// no new content.
//
// wait_for_health, when true, writes a persistent "boot pending
// confirmation" marker to STATE (internal/bootcommit) before switching
// the ESP and rebooting - the *next* boot's own cmd/haproxyosd checks
// it at startup and, in the background, polls HAProxy's own stats
// socket (internal/haproxy.Manager.ShowInfo) until it succeeds
// repeatedly in a row, confirming the marker (internal/
// bootcommit.Confirm) - real, application-level health, not just "the
// daemon process is still running". If that never happens within
// UpgradeRequest.health_timeout_seconds, it reverts back to the slot
// that was active before this Upgrade call and reboots
// (internal/bootrevert.To), autonomously: there is no live caller left
// by then to stream progress back to (the original Upgrade call's
// connection died with the first reboot), so neither the confirmation
// nor a possible revert is ever visible over gRPC, only in the node's
// own logs and the eventual slot it comes back up on. A second,
// independent safety net (rootfs/init's Supervisor.GiveUpAfter) covers
// the one failure mode cmd/haproxyosd's own check can't: haproxyosd
// crashing too fast, or too often, to ever get a chance to run that
// check at all.
func (l *Lifecycle) Upgrade(req *haproxyosv1alpha1.UpgradeRequest, stream haproxyosv1alpha1.LifecycleService_UpgradeServer) error {
	bundleDir := req.GetSource().GetReference()
	if bundleDir == "" {
		return status.Errorf(codes.InvalidArgument, "source.reference is required - a local release bundle directory (see image/release/assemble.sh); real OCI/HTTPS distribution isn't implemented yet")
	}

	send := func(stage string, progress float64, message string) error {
		return stream.Send(&haproxyosv1alpha1.UpgradeResponse{Stage: stage, Progress: progress, Message: message})
	}

	bc, err := resolveBootContext()
	if err != nil {
		return err
	}

	if err := send("verifying", 0.1, fmt.Sprintf("reading release bundle at %s", bundleDir)); err != nil {
		return err
	}

	squashfsPath := filepath.Join(bundleDir, "rootfs.squashfs")
	squashfs, err := os.ReadFile(squashfsPath)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read %s: %v", squashfsPath, err)
	}
	if len(squashfs) > upgradeMaxDataBytes {
		return status.Errorf(codes.FailedPrecondition, "%s is %d bytes, exceeds the %d-byte BOOT-*-DATA partition size", squashfsPath, len(squashfs), upgradeMaxDataBytes)
	}
	if want := req.GetSource().GetSha256(); want != "" {
		if got := sha256Hex(squashfs); got != want {
			return status.Errorf(codes.FailedPrecondition, "%s sha256 %s doesn't match requested %s", squashfsPath, got, want)
		}
	}

	verityPath := filepath.Join(bundleDir, "rootfs.verity")
	verity, err := os.ReadFile(verityPath)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read %s: %v", verityPath, err)
	}
	if len(verity) > upgradeMaxHashBytes {
		return status.Errorf(codes.FailedPrecondition, "%s is %d bytes, exceeds the %d-byte BOOT-*-HASH partition size", verityPath, len(verity), upgradeMaxHashBytes)
	}

	ukiPath := filepath.Join(bundleDir, fmt.Sprintf("uki-%s.efi", strings.ToLower(bc.targetSlot)))
	uki, err := os.ReadFile(ukiPath)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read %s: %v - does this bundle include a UKI for slot %s? (see image/release/assemble.sh)", ukiPath, err, bc.targetSlot)
	}

	targetDataDev, _ := bootslot.SlotDataDevice(bc.disk, bc.targetSlot)
	targetHashDev, _ := bootslot.SlotHashDevice(bc.disk, bc.targetSlot)

	if err := send("writing-data", 0.3, fmt.Sprintf("writing rootfs.squashfs to slot %s (%s)", bc.targetSlot, targetDataDev)); err != nil {
		return err
	}
	if err := writePartitionFile(targetDataDev, squashfs); err != nil {
		return status.Errorf(codes.Internal, "write %s: %v", targetDataDev, err)
	}

	if err := send("writing-hash", 0.5, fmt.Sprintf("writing rootfs.verity to slot %s (%s)", bc.targetSlot, targetHashDev)); err != nil {
		return err
	}
	if err := writePartitionFile(targetHashDev, verity); err != nil {
		return status.Errorf(codes.Internal, "write %s: %v", targetHashDev, err)
	}
	syscall.Sync()

	// Written before the ESP switch below, not after: once the ESP
	// points at bc.targetSlot, that switch has to be protected by a
	// marker already being in place, or a crash in the narrow window
	// between the two would leave an unconfirmed slot active with
	// nothing watching it.
	if req.GetWaitForHealth() {
		if err := bootcommit.Write(&bootcommit.Marker{
			Slot:                 bc.targetSlot,
			RevertTo:             bc.currentSlot,
			TriesLeft:            1,
			HealthTimeoutSeconds: int(req.GetHealthTimeoutSeconds()),
		}); err != nil {
			return status.Errorf(codes.Internal, "write boot-commit marker: %v", err)
		}
	}

	if err := send("switching-slot", 0.8, fmt.Sprintf("switching ESP to slot %s", bc.targetSlot)); err != nil {
		return err
	}
	if err := espswitch.Mount(bc.espDevice); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	writeErr := func() error {
		stagedPath := filepath.Join(espswitch.Mountpoint, "HAPROXYOS", fmt.Sprintf("UKI-%s.EFI", bc.targetSlot))
		if err := os.WriteFile(stagedPath, uki, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", stagedPath, err)
		}
		activePath := filepath.Join(espswitch.Mountpoint, "EFI", "BOOT", "BOOTX64.EFI")
		if err := os.WriteFile(activePath, uki, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", activePath, err)
		}
		return nil
	}()
	if err := espswitch.Unmount(); err != nil {
		log.Printf("lifecycle: unmount %s: %v", espswitch.Mountpoint, err)
	}
	if writeErr != nil {
		return status.Errorf(codes.Internal, "%v", writeErr)
	}
	syscall.Sync()

	if err := send("rebooting", 1.0, fmt.Sprintf("rebooting into slot %s", bc.targetSlot)); err != nil {
		return err
	}

	scheduleReboot()
	return nil
}
