package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Supervisor restarts one child process forever, with a growing backoff
// between restarts that resets after the process has stayed up long
// enough to be considered recovered. This is the "init -> janusd"
// half of PID 1's job (see rootfs/init/README.md) - janusd, in turn,
// supervises haproxy the same way it always has (internal/haproxy.Manager).
type Supervisor struct {
	Path string
	Args []string

	// Stdout/Stderr MUST be a real *os.File (direct fd passthrough to
	// the child) or nil, never a generic io.Writer. os/exec only spawns
	// an internal relay goroutine+pipe when Stdout/Stderr isn't an
	// *os.File - and that goroutine's lifecycle is tied to calling
	// cmd.Wait(), which this package deliberately never does (see
	// runOnce): PID 1 reaps every child through a single shared wait4
	// loop instead, since running cmd.Wait() concurrently with a manual
	// wait4(-1, ...) loop would race both to reap the same pid. A
	// non-*os.File writer here would leak one goroutine per restart,
	// forever.
	Stdout, Stderr *os.File

	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	StableAfter time.Duration // an instance that runs at least this long is "recovered" - backoff resets

	// GiveUpAfter, if nonzero, bounds how long Run keeps restarting the
	// child - measured once, from Run's own start, across every restart
	// attempt (not reset by individual crashes, so a process crashing
	// repeatedly still gives up on schedule rather than getting an
	// ever-renewing budget) - before calling OnGiveUp instead of
	// spawning again and returning. This is the *one* exception to "no
	// give-up threshold, ever" this package otherwise holds to (see the
	// package doc comment): used only by rootfs/init's boot-commit
	// handling, to force a revert to the previous A/B slot when the
	// current one's janusd can't even stay running long enough to
	// run its own HAProxy-level health check (cmd/janusd/main.go,
	// internal/bootcommit.Confirm) rather than restarting a doomed
	// instance forever - a real, different action from "giving up" on
	// reaching the node at all, since the reverted slot is a
	// previously-known-good one. Zero (the default) preserves the
	// unconditional "restart forever" behavior every other caller
	// relies on.
	GiveUpAfter time.Duration
	OnGiveUp    func()
}

// Run starts Path and restarts it every time it exits, forever. Never
// returns - call it from its own goroutine or accept that it's the last
// thing this program does.
func (s *Supervisor) Run() {
	backoff := s.MinBackoff
	var giveUpDeadline time.Time
	if s.GiveUpAfter > 0 {
		giveUpDeadline = time.Now().Add(s.GiveUpAfter)
	}
	for {
		_, backoff = s.runOnce(backoff)
		if !giveUpDeadline.IsZero() && !time.Now().Before(giveUpDeadline) {
			if s.OnGiveUp != nil {
				s.OnGiveUp()
			}
			return
		}
		time.Sleep(backoff)
	}
}

// runOnce spawns the process, blocks until it exits (reaping any other
// already-exited child it encounters along the way without restarting
// them - they're orphans reparented to us, not something this Supervisor
// is tracking), and returns its pid and the backoff to use before the
// next restart. Exported to the package (not just Run) so it can be
// exercised directly against a real short-lived process in tests,
// without needing to wait out a real restart loop.
func (s *Supervisor) runOnce(backoff time.Duration) (pid int, nextBackoff time.Duration) {
	pid, startedAt := s.spawn()
	if pid < 0 {
		return -1, s.growBackoff(backoff)
	}

	for {
		var ws syscall.WaitStatus
		reaped, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if reaped != pid {
			continue // an orphaned grandchild, not the process we're supervising
		}

		fmt.Printf("init: %s (pid %d) exited (%v)\n", s.Path, reaped, ws)
		if time.Since(startedAt) >= s.StableAfter {
			return pid, s.MinBackoff
		}
		return pid, s.growBackoff(backoff)
	}
}

func (s *Supervisor) spawn() (pid int, startedAt time.Time) {
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Stdout = s.Stdout
	cmd.Stderr = s.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Printf("init: start %s: %v\n", s.Path, err)
		return -1, time.Time{}
	}
	return cmd.Process.Pid, time.Now()
}

func (s *Supervisor) growBackoff(b time.Duration) time.Duration {
	b *= 2
	if b > s.MaxBackoff {
		return s.MaxBackoff
	}
	return b
}
