package bootcommit

import (
	"errors"
	"testing"
	"time"
)

func TestReadNoMarker(t *testing.T) {
	Dir = t.TempDir()

	m, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m != nil {
		t.Fatalf("Read on an empty dir returned %+v, want nil", m)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	Dir = t.TempDir()

	want := &Marker{Slot: "B", RevertTo: "A", TriesLeft: 1, HealthTimeoutSeconds: 30}
	if err := Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got == nil {
		t.Fatal("Read after Write returned nil")
	}
	if *got != *want {
		t.Fatalf("Read = %+v, want %+v", *got, *want)
	}
}

func TestWriteCreatesDir(t *testing.T) {
	Dir = t.TempDir() + "/nested/boot"

	if err := Write(&Marker{Slot: "B", RevertTo: "A", TriesLeft: 1}); err != nil {
		t.Fatalf("Write into a not-yet-existing Dir: %v", err)
	}
	if _, err := Read(); err != nil {
		t.Fatalf("Read back: %v", err)
	}
}

func TestClearNoMarker(t *testing.T) {
	Dir = t.TempDir()

	if err := Clear(); err != nil {
		t.Fatalf("Clear on an empty dir: %v", err)
	}
}

func TestClearRemovesMarker(t *testing.T) {
	Dir = t.TempDir()

	if err := Write(&Marker{Slot: "B", RevertTo: "A", TriesLeft: 1}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	m, err := Read()
	if err != nil {
		t.Fatalf("Read after Clear: %v", err)
	}
	if m != nil {
		t.Fatalf("Read after Clear returned %+v, want nil", m)
	}
}

func TestConfirmHealthyClearsMarker(t *testing.T) {
	Dir = t.TempDir()
	m := &Marker{Slot: "B", RevertTo: "A", TriesLeft: 1}
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var reverted bool
	confirmed, err := Confirm(m,
		func() error { return nil }, // always healthy
		func() error { reverted = true; return nil },
		time.Millisecond, 3, time.Hour)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !confirmed {
		t.Fatal("Confirm returned confirmed=false for an always-healthy check")
	}
	if reverted {
		t.Fatal("revert was called despite the check always being healthy")
	}

	got, err := Read()
	if err != nil {
		t.Fatalf("Read after Confirm: %v", err)
	}
	if got != nil {
		t.Fatalf("marker still present after a confirmed Confirm: %+v", got)
	}
}

func TestConfirmRequiresConsecutiveSuccesses(t *testing.T) {
	Dir = t.TempDir()
	m := &Marker{Slot: "B", RevertTo: "A", TriesLeft: 1}
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	calls := 0
	healthy := func() error {
		calls++
		if calls%2 == 0 {
			return errors.New("flaky") // fails every other check, never 3 in a row
		}
		return nil
	}

	confirmed, err := Confirm(m, healthy, func() error { return nil }, time.Millisecond, 3, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if confirmed {
		t.Fatal("Confirm reported confirmed=true despite never getting 3 consecutive successes")
	}
}

func TestConfirmRevertsOnTimeout(t *testing.T) {
	Dir = t.TempDir()
	m := &Marker{Slot: "B", RevertTo: "A", TriesLeft: 1}
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var reverted bool
	confirmed, err := Confirm(m,
		func() error { return errors.New("never healthy") },
		func() error { reverted = true; return nil },
		time.Millisecond, 3, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if confirmed {
		t.Fatal("Confirm reported confirmed=true for a check that never succeeds")
	}
	if !reverted {
		t.Fatal("revert was never called after the timeout elapsed")
	}
}

func TestConfirmPropagatesRevertError(t *testing.T) {
	Dir = t.TempDir()
	m := &Marker{Slot: "B", RevertTo: "A", TriesLeft: 1}
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	wantErr := errors.New("revert failed")
	_, err := Confirm(m,
		func() error { return errors.New("never healthy") },
		func() error { return wantErr },
		time.Millisecond, 3, 10*time.Millisecond)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Confirm error = %v, want %v", err, wantErr)
	}
}

func TestConfirmUsesMarkerTimeoutOverDefault(t *testing.T) {
	Dir = t.TempDir()
	// A marker-specified timeout shorter than defaultTimeout must win -
	// otherwise a per-Upgrade health_timeout_seconds request would be
	// silently ignored in favor of the caller's own fallback.
	m := &Marker{Slot: "B", RevertTo: "A", TriesLeft: 1, HealthTimeoutSeconds: 1}
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	start := time.Now()
	_, _ = Confirm(m, func() error { return errors.New("never healthy") }, func() error { return nil },
		time.Millisecond, 3, time.Hour)
	elapsed := time.Since(start)

	if elapsed >= time.Hour {
		t.Fatalf("Confirm waited the default timeout (1h) instead of the marker's own HealthTimeoutSeconds (1s); elapsed=%v", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Confirm took %v, expected it to revert around the marker's 1s timeout", elapsed)
	}
}

func TestOverwrite(t *testing.T) {
	Dir = t.TempDir()

	if err := Write(&Marker{Slot: "B", RevertTo: "A", TriesLeft: 1}); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := Write(&Marker{Slot: "B", RevertTo: "A", TriesLeft: 0}); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	got, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.TriesLeft != 0 {
		t.Fatalf("TriesLeft = %d after overwrite, want 0", got.TriesLeft)
	}
}
