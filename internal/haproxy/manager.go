// Package haproxy manages a single supervised HAProxy process: validating
// and applying config (via `haproxy -c`), starting it, and reloading it
// seamlessly (`-sf <old pid>`) when the config changes. It deliberately
// does not reimplement HAProxy's own metrics - see docs/architecture.md:
// HAProxy's built-in Prometheus exporter stays the source of truth for
// HAProxy metrics, this package only proxies the runtime (stats) socket
// for HAProxyService's other RPCs (ShowInfo, ServerSetState, ...).
package haproxy

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Manager supervises one HAProxy process instance.
type Manager struct {
	BinaryPath      string
	ConfigPath      string
	PidPath         string
	StatsSocketPath string

	mu  sync.Mutex
	cmd *exec.Cmd
}

func NewManager(binaryPath, configPath, pidPath, statsSocketPath string) *Manager {
	return &Manager{
		BinaryPath:      binaryPath,
		ConfigPath:      configPath,
		PidPath:         pidPath,
		StatsSocketPath: statsSocketPath,
	}
}

// Validate runs `haproxy -c -f <tmpfile>` against cfg without touching the
// running process or ConfigPath. Returns (true, nil) if valid, or
// (false, <haproxy's own error lines>) otherwise.
func (m *Manager) Validate(cfg []byte) (bool, []string) {
	tmp, err := os.CreateTemp("", "haproxyos-validate-*.cfg")
	if err != nil {
		return false, []string{err.Error()}
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(cfg); err != nil {
		tmp.Close()
		return false, []string{err.Error()}
	}
	tmp.Close()

	out, err := exec.Command(m.BinaryPath, "-c", "-f", tmp.Name()).CombinedOutput()
	if err != nil {
		return false, splitNonEmptyLines(string(out))
	}
	return true, nil
}

// Apply validates cfg, writes it to ConfigPath on success, and
// starts/reloads the process. Returns the validation errors (config
// untouched, process untouched) if cfg is invalid.
func (m *Manager) Apply(cfg []byte) ([]string, error) {
	if ok, errs := m.Validate(cfg); !ok {
		return errs, fmt.Errorf("invalid config")
	}
	if err := os.WriteFile(m.ConfigPath, cfg, 0o644); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	return nil, m.startOrReload()
}

// Reload re-applies whatever is currently on disk at ConfigPath - used
// when the config file hasn't changed but a restart is still wanted (or
// as the second half of Apply).
func (m *Manager) Reload() error {
	return m.startOrReload()
}

// startOrReload starts haproxy if it isn't running yet, or performs a
// seamless reload (-sf <old pid>) if it already is. Foreground, no -D:
// the caller (haproxyosd, itself supervised by rootfs/init) is
// responsible for treating this as a managed child process, not letting
// HAProxy detach on its own.
func (m *Manager) startOrReload() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	args := []string{"-f", m.ConfigPath, "-p", m.PidPath}
	if oldPID, err := os.ReadFile(m.PidPath); err == nil {
		if pid := strings.TrimSpace(string(oldPID)); pid != "" {
			args = append(args, "-sf", pid)
		}
	}

	cmd := exec.Command(m.BinaryPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start haproxy: %w", err)
	}
	m.cmd = cmd
	return nil
}

// Info is the subset of `show info` this package parses.
type Info struct {
	Version            string
	UptimeSeconds      uint64
	CurrentConnections uint32
	MaxConnections     uint32
}

// ShowInfo runs the stats socket's "show info" command.
func (m *Manager) ShowInfo() (*Info, error) {
	out, err := m.statsCommand("show info")
	if err != nil {
		return nil, err
	}
	info := &Info{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "Version":
			info.Version = value
		case "Uptime_sec":
			info.UptimeSeconds, _ = strconv.ParseUint(value, 10, 64)
		case "CurrConns":
			v, _ := strconv.ParseUint(value, 10, 32)
			info.CurrentConnections = uint32(v)
		case "MaxConn":
			v, _ := strconv.ParseUint(value, 10, 32)
			info.MaxConnections = uint32(v)
		}
	}
	return info, nil
}

// ShowStat runs the stats socket's "show stat" command, returning the
// raw CSV HAProxy produces (see haproxyos.v1alpha1.HAProxyStatsResponse -
// this package deliberately doesn't parse it structurally yet).
func (m *Manager) ShowStat() ([]byte, error) {
	return m.statsCommand("show stat")
}

// SetServerState runs the stats socket's "set server <backend>/<server>
// state <ready|drain|maint>" command.
func (m *Manager) SetServerState(backend, server, state string) error {
	return mustEmpty(m.statsCommand(fmt.Sprintf("set server %s/%s state %s", backend, server, state)))
}

func (m *Manager) statsCommand(cmd string) ([]byte, error) {
	conn, err := net.DialTimeout("unix", m.StatsSocketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial stats socket: %w", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return nil, fmt.Errorf("write stats command: %w", err)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, conn); err != nil {
		return nil, fmt.Errorf("read stats response: %w", err)
	}
	return buf.Bytes(), nil
}

func splitNonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
