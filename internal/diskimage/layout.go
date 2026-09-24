// Package diskimage computes the fixed six-partition byte layout
// LifecycleService.Install lays a fresh disk out with - the same
// convention image/disk/assemble.sh's own sgdisk invocation produces at
// build time (see internal/bootslot's own doc comment: 1:ESP,
// 2/3:BOOT-A-DATA/HASH, 4/5:BOOT-B-DATA/HASH, 6:STATE), computed here in
// pure Go instead of shelling out, since sgdisk doesn't exist on the
// target OS any more than it does at runtime for Rollback/Upgrade.
//
// Deliberately just the arithmetic - actually writing the GPT table and
// filesystems (github.com/diskfs/go-diskfs) lives in
// internal/api/lifecycle.go's Install, its only real consumer; this
// package exists on its own because the layout math is worth testing
// without needing a real disk or root to do it, the same reasoning
// internal/bootslot's pure cmdline parsing was kept separate from the
// mount/reboot syscalls that act on what it parses.
package diskimage

import "fmt"

// Fixed partition sizes, matching image/disk/assemble.sh's own
// DATA_MB/HASH_MB/ESP_MB - kept identical so a disk Install produces is
// indistinguishable in shape from one assembled at build time, and so
// internal/bootslot's fixed partition-number convention keeps holding
// regardless of which one produced a given disk.
const (
	ESPMB  = 64
	DataMB = 64
	HashMB = 4

	// MinStateMB is the smallest STATE partition Install will accept -
	// below this, the target disk is rejected outright (Compute
	// returns an error) rather than producing a technically-bootable
	// but barely-usable node.
	MinStateMB = 16

	// SectorSize matches every other disk-building script in this
	// project (image/disk/assemble.sh, rootfs/state-image.sh, ...) -
	// 512-byte logical sectors, the same default image/disk/
	// assemble.sh's sgdisk invocation assumes.
	SectorSize = 512
	// AlignSectors is 1MiB, matching sgdisk's own default alignment -
	// keeping it identical isn't load-bearing (nothing here shells out
	// to sgdisk to cross-check), but there's no reason to diverge from
	// a well-established default either.
	AlignSectors = 2048
)

// Layout is the sector-aligned start/end (inclusive, matching GPT's own
// convention and gpt.Partition's Start/End fields) of each of the six
// fixed partitions on a disk of a given total size.
type Layout struct {
	ESPStart, ESPEnd     uint64
	ADataStart, ADataEnd uint64
	AHashStart, AHashEnd uint64
	BDataStart, BDataEnd uint64
	BHashStart, BHashEnd uint64
	StateStart, StateEnd uint64
}

func mb(n int64) int64 { return n * 1024 * 1024 }

func alignUp(sector uint64) uint64 {
	return ((sector + AlignSectors - 1) / AlignSectors) * AlignSectors
}

// Compute lays the six partitions out sequentially, each sector-aligned
// to AlignSectors, giving STATE whatever's left after the ESP and the
// four fixed-size boot partitions - unlike image/disk/assemble.sh's own
// build-time STATE_MB constant (fixed, because that script only ever
// targets a deliberately small QEMU test disk), a real target disk's
// remaining space is put to use instead of wasted.
//
// Returns an error, rather than a Layout, if diskBytes can't fit the
// ESP, both boot slots, and at least MinStateMB of STATE - a target
// disk too small for a real install, not something to silently degrade
// into a broken one.
func Compute(diskBytes int64) (*Layout, error) {
	cur := uint64(AlignSectors)
	next := func(sizeMB int64) (start, end uint64) {
		sectors := uint64(mb(sizeMB) / SectorSize)
		start = cur
		end = start + sectors - 1
		cur = alignUp(end + 1)
		return start, end
	}

	l := &Layout{}
	l.ESPStart, l.ESPEnd = next(ESPMB)
	l.ADataStart, l.ADataEnd = next(DataMB)
	l.AHashStart, l.AHashEnd = next(HashMB)
	l.BDataStart, l.BDataEnd = next(DataMB)
	l.BHashStart, l.BHashEnd = next(HashMB)

	usedBytes := int64(cur) * SectorSize
	remaining := diskBytes - usedBytes
	if remaining < mb(MinStateMB) {
		minTotalMB := float64(usedBytes+mb(MinStateMB)) / 1024 / 1024
		return nil, fmt.Errorf("disk is too small: only %d bytes would be left for STATE (need at least %dMiB) - disk must be at least %.1fMiB total", remaining, MinStateMB, minTotalMB)
	}
	l.StateStart = cur
	l.StateEnd = l.StateStart + uint64(remaining/SectorSize) - 1

	return l, nil
}
