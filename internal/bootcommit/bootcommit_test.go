package bootcommit

import "testing"

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
