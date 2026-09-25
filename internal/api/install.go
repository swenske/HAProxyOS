package api

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	diskfs "github.com/diskfs/go-diskfs"
	diskpkg "github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/bootslot"
	"github.com/swenske/Janus/internal/diskimage"
)

// Install writes a full Janus image to a blank disk for the first
// time - partitioning it from scratch (internal/diskimage.Compute for
// the byte layout, github.com/diskfs/go-diskfs to actually write the
// GPT table and filesystems) rather than shelling out to sgdisk/mtools/
// mkfs.ext4, none of which exist on the target OS any more than they do
// at runtime for Rollback/Upgrade - and unlike those two, Install has
// no existing partition table to build on, so it can't get away with
// Rollback/Upgrade's own trick of only ever moving pre-built UKI bytes
// into place: here it has to lay out the *entire* disk itself, ESP
// filesystem included. It still never builds a UKI, though -
// req.Source.Reference is a local release bundle directory (the same
// shape Upgrade's is: rootfs.squashfs/rootfs.verity/uki-a.efi/
// uki-b.efi, see image/release/assemble.sh), and both slots' UKIs come
// from there pre-built, exactly like Upgrade's does for its one target
// slot.
//
// Both A/B slots get identical content - there's no "other slot" to
// leave untouched yet, the same starting point image/disk/assemble.sh
// itself produces at build time. Slot A is made active by default
// (written to \EFI\BOOT\BOOTX64.EFI); nothing here reboots anything -
// the disk Install just wrote isn't necessarily the one this node
// booted from (see req.Disk's own field comment - it's a caller-chosen
// target), so getting a machine to actually boot from it is the
// caller/operator's job.
//
// Refuses two disks: one that already looks like a Janus install
// (an existing GPT with a recognizable partition name - "use Upgrade/
// Rollback instead" per the proto's own doc comment), and the disk this
// node is itself currently booted from (repartitioning that out from
// under a running system would be catastrophic, and it's Upgrade's
// territory anyway).
func (l *Lifecycle) Install(req *janusv1alpha1.InstallRequest, stream janusv1alpha1.LifecycleService_InstallServer) error {
	diskPath := req.GetDisk()
	if diskPath == "" {
		return status.Errorf(codes.InvalidArgument, "disk is required")
	}
	bundleDir := req.GetSource().GetReference()
	if bundleDir == "" {
		return status.Errorf(codes.InvalidArgument, "source.reference is required - a local release bundle directory (see image/release/assemble.sh); real OCI/HTTPS distribution isn't implemented yet")
	}
	if req.GetControllerAddress() != "" && len(req.GetControllerCaCert()) == 0 {
		return status.Errorf(codes.InvalidArgument, "controller_ca_cert is required whenever controller_address is set - the node has to already know which CA to trust before it ever dials the Controller")
	}

	send := func(stage string, progress float64, message string) error {
		return stream.Send(&janusv1alpha1.InstallResponse{Stage: stage, Progress: progress, Message: message})
	}

	if err := send("verifying", 0.05, fmt.Sprintf("checking %s and reading release bundle at %s", diskPath, bundleDir)); err != nil {
		return err
	}

	if err := refuseIfCurrentBootDisk(diskPath); err != nil {
		return err
	}

	squashfsPath := filepath.Join(bundleDir, "rootfs.squashfs")
	squashfs, err := os.ReadFile(squashfsPath)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read %s: %v", squashfsPath, err)
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
	ukiAPath := filepath.Join(bundleDir, "uki-a.efi")
	ukiA, err := os.ReadFile(ukiAPath)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read %s: %v", ukiAPath, err)
	}
	ukiBPath := filepath.Join(bundleDir, "uki-b.efi")
	ukiB, err := os.ReadFile(ukiBPath)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read %s: %v", ukiBPath, err)
	}
	if len(squashfs) > diskimage.DataMB*1024*1024 {
		return status.Errorf(codes.FailedPrecondition, "%s is %d bytes, exceeds the %d-byte BOOT-*-DATA partition size", squashfsPath, len(squashfs), diskimage.DataMB*1024*1024)
	}
	if len(verity) > diskimage.HashMB*1024*1024 {
		return status.Errorf(codes.FailedPrecondition, "%s is %d bytes, exceeds the %d-byte BOOT-*-HASH partition size", verityPath, len(verity), diskimage.HashMB*1024*1024)
	}

	disk, err := diskfs.Open(diskPath, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "open disk %s: %v", diskPath, err)
	}

	if err := refuseIfAlreadyInstalled(disk, diskPath); err != nil {
		return err
	}

	layout, err := diskimage.Compute(disk.Size)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%s: %v", diskPath, err)
	}

	if err := send("partitioning", 0.15, fmt.Sprintf("writing GPT partition table to %s", diskPath)); err != nil {
		return err
	}
	table := &gpt.Table{
		ProtectiveMBR: true,
		Partitions: []*gpt.Partition{
			{Index: 1, Start: layout.ESPStart, End: layout.ESPEnd, Type: gpt.EFISystemPartition, Name: "ESP"},
			{Index: 2, Start: layout.ADataStart, End: layout.ADataEnd, Type: gpt.LinuxFilesystem, Name: "BOOT-A-DATA"},
			{Index: 3, Start: layout.AHashStart, End: layout.AHashEnd, Type: gpt.LinuxFilesystem, Name: "BOOT-A-HASH"},
			{Index: 4, Start: layout.BDataStart, End: layout.BDataEnd, Type: gpt.LinuxFilesystem, Name: "BOOT-B-DATA"},
			{Index: 5, Start: layout.BHashStart, End: layout.BHashEnd, Type: gpt.LinuxFilesystem, Name: "BOOT-B-HASH"},
			{Index: 6, Start: layout.StateStart, End: layout.StateEnd, Type: gpt.LinuxFilesystem, Name: "STATE"},
		},
	}
	if err := disk.Partition(table); err != nil {
		return status.Errorf(codes.Internal, "write partition table: %v", err)
	}

	if err := send("writing-data", 0.35, "writing rootfs to both A/B slots"); err != nil {
		return err
	}
	for _, start := range []uint64{layout.ADataStart, layout.BDataStart} {
		if err := writeDiskAt(disk, start, squashfs); err != nil {
			return status.Errorf(codes.Internal, "write rootfs.squashfs at sector %d: %v", start, err)
		}
	}
	for _, start := range []uint64{layout.AHashStart, layout.BHashStart} {
		if err := writeDiskAt(disk, start, verity); err != nil {
			return status.Errorf(codes.Internal, "write rootfs.verity at sector %d: %v", start, err)
		}
	}

	if err := send("formatting-state", 0.6, "creating the persistent STATE filesystem"); err != nil {
		return err
	}
	stateFS, err := disk.CreateFilesystem(diskpkg.FilesystemSpec{Partition: 6, FSType: filesystem.TypeExt4, VolumeLabel: "janus-state"})
	if err != nil {
		return status.Errorf(codes.Internal, "create STATE filesystem: %v", err)
	}
	if err := writeControllerConfig(stateFS, req); err != nil {
		return err
	}

	if err := send("writing-esp", 0.8, "building the ESP"); err != nil {
		return err
	}
	espFS, err := disk.CreateFilesystem(diskpkg.FilesystemSpec{Partition: 1, FSType: filesystem.TypeFat32, VolumeLabel: "ESP"})
	if err != nil {
		return status.Errorf(codes.Internal, "create ESP filesystem: %v", err)
	}
	if err := espFS.Mkdir("/EFI/BOOT"); err != nil {
		return status.Errorf(codes.Internal, "mkdir /EFI/BOOT: %v", err)
	}
	if err := espFS.Mkdir("/JANUS"); err != nil {
		return status.Errorf(codes.Internal, "mkdir /JANUS: %v", err)
	}
	if err := writeFSFile(espFS, "/JANUS/UKI-A.EFI", ukiA); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	if err := writeFSFile(espFS, "/JANUS/UKI-B.EFI", ukiB); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	// Slot A active by default - the same starting point image/disk/
	// assemble.sh's own default ACTIVE_SLOT produces.
	if err := writeFSFile(espFS, "/EFI/BOOT/BOOTX64.EFI", ukiA); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}

	syscall.Sync()

	return send("done", 1.0, fmt.Sprintf("installed to %s (slot A active) - reboot the machine into it when ready", diskPath))
}

// refuseIfCurrentBootDisk rejects an Install targeting the disk this
// node is itself currently running from - Install is for provisioning
// a *different*, blank disk (see its own doc comment); the currently-
// active one is Upgrade/Rollback's territory. Never fails the RPC on
// its own account: if there's nothing to compare against (no /proc/
// cmdline, or a cmdline that doesn't parse as a recognized A/B boot -
// e.g. this node itself isn't running a real Janus install at all),
// there's simply nothing to refuse.
func refuseIfCurrentBootDisk(diskPath string) error {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return nil
	}
	currentDisk, ok := bootslot.Disk(dataDev)
	if !ok {
		return nil
	}
	if currentDisk == diskPath {
		return status.Errorf(codes.FailedPrecondition, "%s is the disk this node is currently booted from - Install is for provisioning a different, blank disk; use Upgrade to update this one", diskPath)
	}
	return nil
}

// refuseIfAlreadyInstalled rejects a disk that already has a partition
// carrying one of Janus's own conventional GPT names (see image/
// disk/assemble.sh) - the same signal a human running sgdisk -p would
// use to recognize one. A disk with no partition table at all (d.Table
// == nil, the common "genuinely blank" case) or some unrelated
// non-Janus layout passes through untouched: Install is a bare-
// metal provisioner and is expected to repartition whatever it's
// pointed at, same as any other OS installer - the one thing it must
// not do is destroy a disk that's already a working Janus node.
func refuseIfAlreadyInstalled(d *diskpkg.Disk, diskPath string) error {
	if d.Table == nil {
		return nil
	}
	for _, p := range d.Table.GetPartitions() {
		switch p.Label() {
		case "ESP", "BOOT-A-DATA", "BOOT-A-HASH", "BOOT-B-DATA", "BOOT-B-HASH", "STATE":
			return status.Errorf(codes.FailedPrecondition, "%s already has a %q partition - looks like an existing Janus install; use Upgrade/Rollback instead, or wipe the disk first if this is intentional", diskPath, p.Label())
		}
	}
	return nil
}

// writeDiskAt writes data starting at the given sector, straight to the
// disk's backing storage - the same raw-partition-write approach
// internal/api/lifecycle.go's Upgrade already uses (writePartitionFile)
// for an already-partitioned disk's device nodes; here there are no
// device nodes yet (the kernel hasn't re-read this partition table), so
// this writes by byte offset into the disk itself instead.
func writeDiskAt(d *diskpkg.Disk, startSector uint64, data []byte) error {
	w, err := d.Backend.Writable()
	if err != nil {
		return err
	}
	_, err = w.WriteAt(data, int64(startSector)*diskimage.SectorSize)
	return err
}

func writeFSFile(fs filesystem.FileSystem, path string, data []byte) error {
	f, err := fs.OpenFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeControllerConfig writes the node self-registration config
// (controller_address/controller_ca_cert, see InstallRequest's own doc
// comment) onto the freshly-created STATE filesystem, under a
// controller/ directory - the same one-directory-per-concern convention
// STATE already uses on a running node (pki/, haproxy/, boot/, see
// rootfs/init/main.go's mountState), just written here directly through
// go-diskfs rather than a bind mount, since STATE doesn't exist as a
// mountable device node yet at Install time. A future janusd boot reads
// this back to decide whether to self-register - not built yet (Point 2
// suite tranche 5); this tranche is scoped to Install writing the files
// correctly, verified directly off the resulting STATE filesystem
// (hack/lifecycle-install-test.sh), not by booting and having anything
// consume them. A blank controller_address (the common case - most
// installs have no Controller at all) writes nothing, same as every
// install before this field existed.
func writeControllerConfig(fs filesystem.FileSystem, req *janusv1alpha1.InstallRequest) error {
	addr := req.GetControllerAddress()
	if addr == "" {
		return nil
	}
	// No leading slash, unlike the ESP (FAT32) paths elsewhere in this
	// file: go-diskfs's ext4 driver runs Mkdir's path through Go's
	// io/fs.ValidPath, which rejects a leading "/" outright
	// (io.fs.ErrInvalid, "invalid argument") - found by a real Install
	// call failing with exactly that error, not assumed from the
	// FAT32 convention already used above.
	if err := fs.Mkdir("controller"); err != nil {
		return status.Errorf(codes.Internal, "mkdir STATE controller/: %v", err)
	}
	if err := writeFSFile(fs, "controller/address", []byte(addr)); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	if err := writeFSFile(fs, "controller/ca.crt", req.GetControllerCaCert()); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	return nil
}
