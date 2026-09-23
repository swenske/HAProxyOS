package main

import "strings"

// dmVerityDataDevice extracts the verity target's data device (e.g.
// "/dev/vda1") from a raw kernel cmdline string containing a
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
func dmVerityDataDevice(cmdline string) (string, bool) {
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

// statePartitionDevice derives the STATE partition's device path from
// the root verity data device, by the fixed convention
// image/disk/assemble.sh's own GPT layout uses: STATE is always
// partition 5 on the same disk BOOT-A/BOOT-B live on. Only applies when
// dataDev is itself a partition (ends in a digit, e.g. "/dev/vda1" -
// the real, single-disk layout); returns ok=false for a bare
// whole-disk device (e.g. "/dev/vda" - hack/qemu-verity-boot-test.sh's
// and hack/qemu-state-persist-test.sh's separate-virtio-blk-drives
// harness, which has no partition table on the root device at all, and
// deliberately keeps working that way - see mountState's doc comment).
func statePartitionDevice(dataDev string) (string, bool) {
	i := len(dataDev)
	for i > 0 && dataDev[i-1] >= '0' && dataDev[i-1] <= '9' {
		i--
	}
	if i == len(dataDev) {
		return "", false // no trailing digit at all - a whole-disk device
	}
	disk := dataDev[:i]
	if disk == "" {
		return "", false
	}
	const statePartitionNumber = "5" // image/disk/assemble.sh: partition 5 is always STATE
	return disk + statePartitionNumber, true
}
