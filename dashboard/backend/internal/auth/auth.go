// Package auth gates the Janus Controller's main UI (dashboardd's -addr
// port) behind a single admin password - forced setup on first run, a
// session cookie afterward. There is exactly one account: this is a
// single-operator tool, not a multi-user system, so a username/roles
// model would be unused complexity.
//
// The main port stays plain HTTP by design (see dashboard/backend/
// main.go's own doc comment - "nothing sensitive transits here" no
// longer fully holds once a password and a session cookie cross it, but
// upgrading this port to TLS is a real architecture change of its own,
// not bundled into this one - the honest interim position, matching how
// self-hosted admin tools commonly work, is: this port is meant for a
// trusted network, and anyone exposing it more broadly should put a
// TLS-terminating reverse proxy in front of it. Documented as a known
// limitation, not hidden.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	credentialsFile = "auth.json"
	sessionTTL      = 24 * time.Hour
	minPasswordLen  = 8
)

// Store holds the single admin credential and in-memory sessions.
// Sessions are deliberately not persisted - a restart requires signing
// in again, a reasonable default for an admin tool and far simpler than
// a real refresh-token scheme.
type Store struct {
	path string

	mu           sync.Mutex
	passwordHash []byte // nil until setup completes
	sessions     map[string]time.Time
}

type credentialsFileContents struct {
	PasswordHash []byte `json:"password_hash"`
}

// Open loads existing credentials from dataDir, or leaves the store
// empty (setup required) if none exist yet.
func Open(dataDir string) (*Store, error) {
	s := &Store{path: dataDir + "/" + credentialsFile, sessions: map[string]time.Time{}}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	var c credentialsFileContents
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.passwordHash = c.PasswordHash
	return s, nil
}

// SetupRequired reports whether no admin password has been set yet.
func (s *Store) SetupRequired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.passwordHash == nil
}

// Setup sets the admin password for the first time - fails if one
// already exists (use ChangePassword for that, not built here yet, no
// caller needs it before the UI itself can prompt for it).
func (s *Store) Setup(password string) error {
	if len(password) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.passwordHash != nil {
		return fmt.Errorf("admin password already set")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	data, err := json.Marshal(credentialsFileContents{PasswordHash: hash})
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}

	s.passwordHash = hash
	return nil
}

// Verify checks password against the stored hash. Returns false (not an
// error) for a plain wrong password - only a real I/O/state problem is
// an error.
func (s *Store) Verify(password string) bool {
	s.mu.Lock()
	hash := s.passwordHash
	s.mu.Unlock()
	if hash == nil {
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

// NewSession issues a fresh session token, valid for sessionTTL.
func (s *Store) NewSession() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	s.sessions[token] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	return token, nil
}

// ValidSession reports whether token is a live, unexpired session -
// expired entries are pruned as a side effect, since there's no
// background sweep for this deliberately small, in-memory map.
func (s *Store) ValidSession(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// Revoke ends one session (logout).
func (s *Store) Revoke(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}
