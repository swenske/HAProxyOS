package api

import (
	"context"
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
	"github.com/swenske/HAProxyOS/internal/bootslot"
)

// Lifecycle implements haproxyosv1alpha1.LifecycleServiceServer. Install
// isn't implemented yet (bare-metal provisioning of a *fresh*, unpartitioned
// disk needs a Go-native GPT/FAT builder - sgdisk/mtools/ukify don't exist
// on the target OS any more than they do at runtime for Rollback/Upgrade
// below, and unlike those two, Install has no existing partition table to
// build on). Upgrade.wait_for_health isn't implemented yet either (needs a
// persistent "boot pending confirmation" marker the *next* boot checks and
// clears, plus an automatic revert if it never does - see docs/
// architecture.md's Phase 3 notes) - see its own doc comment.
//
// Rollback and Upgrade share the same core trick: the running node can
// never shell out to `ukify`/`sbsign` (no package manager, by design), so
// both just move already-built UKIs into place instead of ever assembling
// one. Rollback moves a UKI image/disk/activate-slot.sh already staged at
// build/install time (\HAPROXYOS\UKI-A.EFI / UKI-B.EFI); Upgrade moves one
// that arrived as part of a "release bundle" (image/release/assemble.sh) -
// which exists specifically because a genuinely *new* rootfs has a root
// hash nobody could have pre-staged at the original install's build time.
type Lifecycle struct {
	haproxyosv1alpha1.UnimplementedLifecycleServiceServer
}

// espMountpoint is under /run, which rootfs/init/main.go's
// mountEphemeral already made a fresh tmpfs - nothing pre-existing
// there needs preserving, unlike /etc (see that function's own doc
// comment).
const espMountpoint = "/run/haproxyos/esp"

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

func mountESP(espDev string) error {
	if err := os.MkdirAll(espMountpoint, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", espMountpoint, err)
	}
	if err := syscall.Mount(espDev, espMountpoint, "vfat", 0, ""); err != nil {
		return fmt.Errorf("mount ESP %s: %w", espDev, err)
	}
	return nil
}

func unmountESP() {
	if err := syscall.Unmount(espMountpoint, 0); err != nil {
		log.Printf("lifecycle: unmount %s: %v", espMountpoint, err)
	}
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

	if err := mountESP(bc.espDevice); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	defer unmountESP()

	src := filepath.Join(espMountpoint, "HAPROXYOS", fmt.Sprintf("UKI-%s.EFI", bc.targetSlot))
	staged, err := os.ReadFile(src)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read staged UKI for slot %s (%s): %v - was image/disk/activate-slot.sh ever run for this disk?", bc.targetSlot, src, err)
	}
	dst := filepath.Join(espMountpoint, "EFI", "BOOT", "BOOTX64.EFI")
	if err := os.WriteFile(dst, staged, 0o644); err != nil {
		return nil, status.Errorf(codes.Internal, "write %s: %v", dst, err)
	}
	syscall.Sync()

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
// wait_for_health isn't implemented: Upgrade always reboots immediately,
// with no automatic rollback if the new slot fails to come up healthy.
// Real automatic rollback needs a persistent "boot pending confirmation"
// marker on STATE that survives the reboot, which the *next* boot's own
// startup path checks and either clears (success) or - if it's still set
// after some prior boot never got the chance to clear it, meaning that
// boot crashed or never came up - triggers a revert back to the slot that
// was active before this Upgrade call. None of that bookkeeping exists
// yet, so asking for it now would silently promise something this
// doesn't do.
func (l *Lifecycle) Upgrade(req *haproxyosv1alpha1.UpgradeRequest, stream haproxyosv1alpha1.LifecycleService_UpgradeServer) error {
	if req.GetWaitForHealth() {
		return status.Errorf(codes.Unimplemented, "wait_for_health isn't implemented yet - Upgrade always reboots immediately with no post-boot health check")
	}

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

	if err := send("switching-slot", 0.8, fmt.Sprintf("switching ESP to slot %s", bc.targetSlot)); err != nil {
		return err
	}
	if err := mountESP(bc.espDevice); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	writeErr := func() error {
		stagedPath := filepath.Join(espMountpoint, "HAPROXYOS", fmt.Sprintf("UKI-%s.EFI", bc.targetSlot))
		if err := os.WriteFile(stagedPath, uki, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", stagedPath, err)
		}
		activePath := filepath.Join(espMountpoint, "EFI", "BOOT", "BOOTX64.EFI")
		if err := os.WriteFile(activePath, uki, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", activePath, err)
		}
		return nil
	}()
	unmountESP()
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
