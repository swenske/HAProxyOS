package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
	"github.com/swenske/HAProxyOS/internal/bootslot"
)

// Lifecycle implements haproxyosv1alpha1.LifecycleServiceServer.
// Install/Upgrade aren't implemented yet (need to write a genuinely new
// rootfs image into the inactive slot, plus a post-reboot health check
// with automatic rollback) - see roadmap Phase 3 in
// docs/architecture.md. Rollback is real: the gRPC front end for
// exactly the operation image/disk/activate-slot.sh already performs
// at build/install time - switch which A/B slot's Unified Kernel Image
// is active on the ESP - except this runs on an already-booted node,
// which has no package manager and so can never shell out to `ukify`
// the way that script does. That's why activate-slot.sh stages BOTH
// slots' UKIs on the ESP under \HAPROXYOS\UKI-A.EFI/UKI-B.EFI: at
// runtime this is nothing more than mounting the ESP (CONFIG_VFAT_FS)
// and copying the *other* slot's already-built UKI over
// \EFI\BOOT\BOOTX64.EFI.
type Lifecycle struct {
	haproxyosv1alpha1.UnimplementedLifecycleServiceServer
}

// espMountpoint is under /run, which rootfs/init/main.go's
// mountEphemeral already made a fresh tmpfs - nothing pre-existing
// there needs preserving, unlike /etc (see that function's own doc
// comment).
const espMountpoint = "/run/haproxyos/esp"

func (l *Lifecycle) Rollback(_ context.Context, _ *emptypb.Empty) (*haproxyosv1alpha1.RollbackResponse, error) {
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
		return nil, status.Errorf(codes.FailedPrecondition, "root device %q isn't a recognized A/B slot (see image/disk/assemble.sh's fixed partition layout) - can't determine the other slot to roll back to", dataDev)
	}
	espDev, ok := bootslot.ESPDevice(dataDev)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "couldn't derive the ESP device from root device %q", dataDev)
	}
	target := bootslot.OtherSlot(current)

	if err := os.MkdirAll(espMountpoint, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir %s: %v", espMountpoint, err)
	}
	if err := syscall.Mount(espDev, espMountpoint, "vfat", 0, ""); err != nil {
		return nil, status.Errorf(codes.Internal, "mount ESP %s: %v", espDev, err)
	}
	defer syscall.Unmount(espMountpoint, 0)

	src := filepath.Join(espMountpoint, "HAPROXYOS", fmt.Sprintf("UKI-%s.EFI", target))
	staged, err := os.ReadFile(src)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read staged UKI for slot %s (%s): %v - was image/disk/activate-slot.sh ever run for this disk?", target, src, err)
	}
	dst := filepath.Join(espMountpoint, "EFI", "BOOT", "BOOTX64.EFI")
	if err := os.WriteFile(dst, staged, 0o644); err != nil {
		return nil, status.Errorf(codes.Internal, "write %s: %v", dst, err)
	}
	syscall.Sync()

	// The reboot happens after this RPC returns, not before - the
	// caller needs to actually see RollbackResponse rather than the
	// connection just dying mid-call.
	go func() {
		time.Sleep(2 * time.Second)
		syscall.Sync()
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	}()

	return &haproxyosv1alpha1.RollbackResponse{ActiveSlot: target}, nil
}
