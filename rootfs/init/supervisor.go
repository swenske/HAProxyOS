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
// enough to be considered recovered. This is the "init -> haproxyosd"
// half of PID 1's job (see rootfs/init/README.md) - haproxyosd, in turn,
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
}

// Run starts Path and restarts it every time it exits, forever. Never
// returns - call it from its own goroutine or accept that it's the last
// thing this program does.
func (s *Supervisor) Run() {
	backoff := s.MinBackoff
	for {
		_, backoff = s.runOnce(backoff)
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
