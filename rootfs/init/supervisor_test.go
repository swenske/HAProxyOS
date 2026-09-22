package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func lookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available: %v", name, err)
	}
	return path
}

// TestSupervisorRestartsWithNewPID proves runOnce actually spawns a new
// OS process each time, not just returns a plausible-looking result -
// three consecutive calls against a process that exits immediately must
// produce three distinct, valid pids.
func TestSupervisorRestartsWithNewPID(t *testing.T) {
	truePath := lookPath(t, "true")

	s := &Supervisor{
		Path:        truePath,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		MinBackoff:  5 * time.Millisecond,
		MaxBackoff:  20 * time.Millisecond,
		StableAfter: time.Hour, // exits immediately - never "stable" in this test
	}

	seen := map[int]bool{}
	backoff := s.MinBackoff
	for i := 0; i < 3; i++ {
		pid, next := s.runOnce(backoff)
		if pid <= 0 {
			t.Fatalf("run %d: expected a valid pid, got %d", i, pid)
		}
		if seen[pid] {
			t.Fatalf("run %d: pid %d was already seen - not a fresh process", i, pid)
		}
		seen[pid] = true
		backoff = next
	}
}

// TestSupervisorBackoffGrowsOnFastCrashes and
// TestSupervisorBackoffResetsAfterStableRun cover the two halves of the
// backoff policy: a process that keeps crashing immediately should back
// off further each time (bounded by MaxBackoff), but one that runs past
// StableAfter before exiting should reset to MinBackoff rather than
// inheriting a large backoff from an old, unrelated crash loop.
func TestSupervisorBackoffGrowsOnFastCrashes(t *testing.T) {
	truePath := lookPath(t, "true")
	s := &Supervisor{
		Path:        truePath,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		MinBackoff:  5 * time.Millisecond,
		MaxBackoff:  40 * time.Millisecond,
		StableAfter: time.Hour,
	}

	backoff := s.MinBackoff
	var got []time.Duration
	for i := 0; i < 4; i++ {
		_, backoff = s.runOnce(backoff)
		got = append(got, backoff)
	}

	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff sequence = %v, want %v", got, want)
		}
	}
}

func TestSupervisorBackoffResetsAfterStableRun(t *testing.T) {
	shPath := lookPath(t, "sh")
	s := &Supervisor{
		Path:        shPath,
		Args:        []string{"-c", "sleep 0.05"},
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		MinBackoff:  5 * time.Millisecond,
		MaxBackoff:  40 * time.Millisecond,
		StableAfter: 20 * time.Millisecond, // shorter than the 0.05s sleep above
	}

	// Start from an already-grown backoff, as if a previous fast crash
	// loop had happened just before this "stable" run.
	_, next := s.runOnce(20 * time.Millisecond)
	if next != s.MinBackoff {
		t.Fatalf("backoff after a stable run = %v, want it reset to MinBackoff (%v)", next, s.MinBackoff)
	}
}

// TestSupervisorIgnoresUnrelatedChildren proves the wait4 loop only acts
// on the process it's actually supervising - a decoy child (reaped by
// the very same syscall.Wait4(-1, ...) call, since it's a child of this
// test process too) must never be mistaken for the supervised process
// exiting.
func TestSupervisorIgnoresUnrelatedChildren(t *testing.T) {
	truePath := lookPath(t, "true")

	decoy := exec.Command(truePath)
	if err := decoy.Start(); err != nil {
		t.Fatalf("start decoy: %v", err)
	}
	decoyPid := decoy.Process.Pid
	t.Cleanup(func() { _ = decoy.Wait() }) // best-effort - runOnce below may have already reaped it

	s := &Supervisor{
		Path:        truePath,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		MinBackoff:  5 * time.Millisecond,
		MaxBackoff:  20 * time.Millisecond,
		StableAfter: time.Hour,
	}
	pid, _ := s.runOnce(s.MinBackoff)
	if pid == decoyPid {
		t.Fatalf("runOnce returned the decoy's pid (%d) - it should only ever return its own supervised process's pid", decoyPid)
	}
	if pid <= 0 {
		t.Fatalf("expected a valid supervised pid, got %d", pid)
	}
}

// TestSupervisorHandlesSpawnFailure ensures a process that fails to even
// start (nonexistent path) doesn't hang runOnce forever waiting on a
// wait4() for a pid that was never created.
func TestSupervisorHandlesSpawnFailure(t *testing.T) {
	s := &Supervisor{
		Path:        "/nonexistent/binary/does-not-exist",
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		MinBackoff:  5 * time.Millisecond,
		MaxBackoff:  20 * time.Millisecond,
		StableAfter: time.Hour,
	}

	done := make(chan struct{})
	var pid int
	var backoff time.Duration
	go func() {
		pid, backoff = s.runOnce(s.MinBackoff)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runOnce hung after a spawn failure instead of returning")
	}

	if pid != -1 {
		t.Fatalf("pid = %d, want -1 after a spawn failure", pid)
	}
	if backoff != 10*time.Millisecond {
		t.Fatalf("backoff = %v, want it grown to 10ms even on a spawn failure", backoff)
	}
}
