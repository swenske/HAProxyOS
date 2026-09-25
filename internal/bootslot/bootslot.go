// Package bootslot answers "which A/B slot am I running from, and
// where's the rest of the disk (ESP, STATE)" by parsing the running
// kernel's own /proc/cmdline - the one thing every boot mode (rootfs/
// init's real single-disk layout, its older separate-drives test
// harnesses, janusd running on an already-booted node) can read
// without any other input. Used by rootfs/init (mounting STATE at
// boot - see mountState in rootfs/init/main.go) and by
// internal/api/lifecycle.go's LifecycleService.Rollback (finding the
// ESP and the *other* slot to switch to).
//
// The partition layout this package assumes is image/disk/
// assemble.sh's own fixed convention: 1:ESP, 2:BOOT-A-DATA,
// 3:BOOT-A-HASH, 4:BOOT-B-DATA, 5:BOOT-B-HASH, 6:STATE. Nothing here
// discovers that layout generically (e.g. by GPT partition label) -
// this project controls both ends (image assembly and every reader of
// it) and fixes the convention instead, deliberately, the same
// reasoning image/disk/assemble.sh's own comments give for not needing
// /dev/disk/by-partlabel/* (no udev on this system anyway).
package bootslot

import "strings"

// DataDevice extracts the verity target's data device (e.g.
// "/dev/vda2") from a raw kernel cmdline string containing a
// dm-mod.create="..." parameter in the exact form this project's own
// boot scripts generate (see hack/qemu-ab-boot-test.sh,
// hack/qemu-verity-boot-test.sh):
//
//	dm-mod.create="<name>,<uuid>,<minor>,<flags>,<start> <sectors> verity <version> <data_dev> <hash_dev> ..."
//
// /proc/cmdline preserves the quotes verbatim (confirmed by actually
// booting and reading it back, not assumed from the kernel's own
// reformatted dmesg "Command line:" line) - the parser below relies on
// that. Returns ok=false if the parameter isn't present or doesn't
// parse as expected; never panics on a malformed or foreign cmdline
// (e.g. Phase 1/2's initramfs boots, which have no dm-mod.create= at
// all).
func DataDevice(cmdline string) (string, bool) {
	const key = `dm-mod.create="`
	i := strings.Index(cmdline, key)
	if i < 0 {
		return "", false
	}
	rest := cmdline[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return "", false
	}
	value := rest[:j]

	// name,uuid,minor,flags,table - the table itself is space-separated
	// and never contains a comma for the single-mapping cmdlines this
	// project generates, so splitting on "," with a limit of 5 is exact,
	// not just "good enough".
	parts := strings.SplitN(value, ",", 5)
	if len(parts) != 5 {
		return "", false
	}
	fields := strings.Fields(parts[4])
	// 0:start 1:sectors 2:"verity" 3:version 4:data_dev 5:hash_dev ...
	if len(fields) < 5 || fields[2] != "verity" {
		return "", false
	}
	return fields[4], true
}

// diskAndPartition splits a partition device path into its disk and
// partition-number parts (e.g. "/dev/vda2" -> "/dev/vda", "2"). ok is
// false for a bare whole-disk device with no trailing digit at all
// (e.g. "/dev/vda" - the older separate-virtio-blk-drives test
// harnesses, hack/qemu-verity-boot-test.sh and
// hack/qemu-state-persist-test.sh, which have no partition table on
// the root device).
func diskAndPartition(dataDev string) (disk, partition string, ok bool) {
	i := len(dataDev)
	for i > 0 && dataDev[i-1] >= '0' && dataDev[i-1] <= '9' {
		i--
	}
	if i == len(dataDev) || i == 0 {
		return "", "", false
	}
	return dataDev[:i], dataDev[i:], true
}

// StateDevice derives the STATE partition's device path from the root
// verity data device, by the fixed convention image/disk/assemble.sh's
// GPT layout uses: STATE is always partition 6 on the same disk. Only
// applies when dataDev is itself a partition; see diskAndPartition.
func StateDevice(dataDev string) (string, bool) {
	disk, _, ok := diskAndPartition(dataDev)
	if !ok {
		return "", false
	}
	return disk + "6", true
}

// ESPDevice derives the ESP's device path the same way StateDevice
// does - partition 1 on the same disk.
func ESPDevice(dataDev string) (string, bool) {
	disk, _, ok := diskAndPartition(dataDev)
	if !ok {
		return "", false
	}
	return disk + "1", true
}

// ActiveSlot reports which A/B slot dataDev belongs to - "A" for
// partition 2 (BOOT-A-DATA), "B" for partition 4 (BOOT-B-DATA). ok is
// false for anything else, including a whole-disk device or a
// partition number that isn't a recognized BOOT-*-DATA slot (e.g. the
// ESP itself, a hash partition, or STATE).
func ActiveSlot(dataDev string) (string, bool) {
	_, partition, ok := diskAndPartition(dataDev)
	if !ok {
		return "", false
	}
	switch partition {
	case "2":
		return "A", true
	case "4":
		return "B", true
	default:
		return "", false
	}
}

// OtherSlot flips "A"<->"B". Any other input is returned unchanged -
// callers are expected to have already validated slot via ActiveSlot
// or their own input parsing.
func OtherSlot(slot string) string {
	switch slot {
	case "A":
		return "B"
	case "B":
		return "A"
	default:
		return slot
	}
}

// SlotDataDevice and SlotHashDevice derive a *given* slot's own
// data/hash partition devices on the given disk - the reverse
// direction from ActiveSlot (which goes device -> slot, given the
// currently-booted one): these go slot -> device, for a slot that
// isn't necessarily the one currently running (e.g.
// LifecycleService.Upgrade writing to the *inactive* slot). ok is
// false for anything other than "A" or "B".
func SlotDataDevice(disk, slot string) (string, bool) {
	switch slot {
	case "A":
		return disk + "2", true
	case "B":
		return disk + "4", true
	default:
		return "", false
	}
}

func SlotHashDevice(disk, slot string) (string, bool) {
	switch slot {
	case "A":
		return disk + "3", true
	case "B":
		return disk + "5", true
	default:
		return "", false
	}
}

// Disk strips the trailing partition number off a partition device
// path (e.g. "/dev/vda2" -> "/dev/vda") - the same split
// diskAndPartition does internally, exported for callers (like
// LifecycleService.Upgrade) that need the disk itself to derive a
// *different* slot's devices via SlotDataDevice/SlotHashDevice, not
// just the fixed STATE/ESP partitions StateDevice/ESPDevice already
// cover.
func Disk(dataDev string) (string, bool) {
	disk, _, ok := diskAndPartition(dataDev)
	return disk, ok
}
