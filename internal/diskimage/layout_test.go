package diskimage

import "testing"

func TestComputeTypicalDisk(t *testing.T) {
	// 2GiB disk - comfortably larger than the four fixed-size boot
	// partitions plus the ESP, leaving plenty for STATE.
	l, err := Compute(2 * 1024 * 1024 * 1024)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	// Partitions must be in order, non-overlapping, and each sized
	// exactly right.
	checks := []struct {
		name        string
		start, end  uint64
		wantSectors uint64
	}{
		{"ESP", l.ESPStart, l.ESPEnd, uint64(mb(ESPMB) / SectorSize)},
		{"A-DATA", l.ADataStart, l.ADataEnd, uint64(mb(DataMB) / SectorSize)},
		{"A-HASH", l.AHashStart, l.AHashEnd, uint64(mb(HashMB) / SectorSize)},
		{"B-DATA", l.BDataStart, l.BDataEnd, uint64(mb(DataMB) / SectorSize)},
		{"B-HASH", l.BHashStart, l.BHashEnd, uint64(mb(HashMB) / SectorSize)},
	}
	for _, c := range checks {
		got := c.end - c.start + 1
		if got != c.wantSectors {
			t.Errorf("%s: %d sectors, want %d", c.name, got, c.wantSectors)
		}
	}

	starts := []uint64{l.ESPStart, l.ADataStart, l.AHashStart, l.BDataStart, l.BHashStart, l.StateStart}
	ends := []uint64{l.ESPEnd, l.ADataEnd, l.AHashEnd, l.BDataEnd, l.BHashEnd, l.StateEnd}
	for i := 1; i < len(starts); i++ {
		if starts[i] <= ends[i-1] {
			t.Fatalf("partition %d starts (%d) before partition %d ends (%d) - overlap", i, starts[i], i-1, ends[i-1])
		}
		if starts[i]%AlignSectors != 0 {
			t.Errorf("partition %d starts at sector %d, not %d-aligned", i, starts[i], AlignSectors)
		}
	}

	if l.StateEnd <= l.StateStart {
		t.Fatalf("STATE partition is empty or invalid: start=%d end=%d", l.StateStart, l.StateEnd)
	}
	stateMB := float64((l.StateEnd-l.StateStart+1)*SectorSize) / 1024 / 1024
	if stateMB < MinStateMB {
		t.Fatalf("STATE is %.1fMiB, below MinStateMB (%d)", stateMB, MinStateMB)
	}
}

func TestComputeTooSmall(t *testing.T) {
	// Barely bigger than the four fixed boot partitions + ESP alone -
	// nowhere near enough left for MinStateMB.
	tooSmall := int64(ESPMB+2*DataMB+2*HashMB) * 1024 * 1024
	if _, err := Compute(tooSmall); err == nil {
		t.Fatal("Compute succeeded on a disk with no room for STATE, want an error")
	}
}

func TestComputeExactMinimum(t *testing.T) {
	// Just enough for MinStateMB, plus alignment slack.
	minDiskMB := int64(ESPMB+2*DataMB+2*HashMB+MinStateMB) + 4 // slack for alignment rounding
	l, err := Compute(minDiskMB * 1024 * 1024)
	if err != nil {
		t.Fatalf("Compute at the minimum viable size: %v", err)
	}
	if l.StateEnd <= l.StateStart {
		t.Fatal("STATE partition is empty at the minimum viable disk size")
	}
}

func TestComputeLargerDiskGrowsState(t *testing.T) {
	small, err := Compute(2 * 1024 * 1024 * 1024)
	if err != nil {
		t.Fatalf("Compute(2GiB): %v", err)
	}
	large, err := Compute(20 * 1024 * 1024 * 1024)
	if err != nil {
		t.Fatalf("Compute(20GiB): %v", err)
	}

	smallState := small.StateEnd - small.StateStart
	largeState := large.StateEnd - large.StateStart
	if largeState <= smallState {
		t.Fatalf("STATE on a 20GiB disk (%d sectors) isn't bigger than on a 2GiB disk (%d sectors) - remaining space isn't being used", largeState, smallState)
	}
	// Every other partition must be identically sized/positioned
	// regardless of overall disk size - only STATE grows.
	if small.ESPStart != large.ESPStart || small.ESPEnd != large.ESPEnd {
		t.Fatalf("ESP differs between disk sizes: %d-%d vs %d-%d", small.ESPStart, small.ESPEnd, large.ESPStart, large.ESPEnd)
	}
	if small.BHashEnd != large.BHashEnd {
		t.Fatalf("BOOT-B-HASH end differs between disk sizes: %d vs %d", small.BHashEnd, large.BHashEnd)
	}
}
