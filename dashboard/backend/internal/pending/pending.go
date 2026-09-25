// Package pending holds nodes that have self-announced to the
// Controller's registration endpoint (see dashboard/backend's own
// startRegistrationListener) but haven't been approved by a human yet -
// a Tailscale-style admission gate, not fully automatic enrollment
// (the user's explicit choice once what fully-automatic would mean was
// laid out). Persisted to disk, same convention as internal/store: a
// pending entry needs to survive a Controller restart while awaiting
// approval, since the node itself only announces once per boot (see
// the node-side self-registration design) - losing it to an in-memory
// store would strand that node until its next reboot.
package pending

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Node is one self-announced, not-yet-approved node. ServiceCertPEM/
// ServiceKeyPEM are the credential the node generated for *itself* and
// sent during registration - unlike the human-driven add-node flow
// (dashboard/backend/main.go's parseAddNodeRequest), there's no
// separate bootstrap-credential-exchanged-for-a-service-credential
// step here: the node already did that part locally before announcing,
// so this is already the final shape store.Add needs on approval.
type Node struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Address        string    `json:"address"`
	CACertPEM      []byte    `json:"-"`
	ServiceCertPEM []byte    `json:"-"`
	ServiceKeyPEM  []byte    `json:"-"`
	AnnouncedAt    time.Time `json:"announced_at"`
}

// meta is Node's on-disk, non-secret half - same split store.go's own
// meta type uses, cert/key material in sibling files instead of one
// blob mixing secret and non-secret fields.
type meta struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Address     string    `json:"address"`
	AnnouncedAt time.Time `json:"announced_at"`
}

type Store struct {
	dir string

	mu    sync.Mutex
	nodes map[string]*Node
}

// Open loads every already-pending node from dir (creating the pending/
// subdirectory if this is the first run).
func Open(dir string) (*Store, error) {
	pendingDir := filepath.Join(dir, "pending")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", pendingDir, err)
	}

	s := &Store{dir: dir, nodes: map[string]*Node{}}

	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pendingDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		node, err := loadNode(filepath.Join(pendingDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("load pending node %s: %w", entry.Name(), err)
		}
		s.nodes[node.ID] = node
	}
	return s, nil
}

func loadNode(dir string) (*Node, error) {
	metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("read meta.json: %w", err)
	}
	var m meta
	if err := json.Unmarshal(metaBytes, &m); err != nil {
		return nil, fmt.Errorf("parse meta.json: %w", err)
	}

	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read ca.crt: %w", err)
	}
	cert, err := os.ReadFile(filepath.Join(dir, "service.crt"))
	if err != nil {
		return nil, fmt.Errorf("read service.crt: %w", err)
	}
	key, err := os.ReadFile(filepath.Join(dir, "service.key"))
	if err != nil {
		return nil, fmt.Errorf("read service.key: %w", err)
	}

	return &Node{
		ID: m.ID, Name: m.Name, Address: m.Address, AnnouncedAt: m.AnnouncedAt,
		CACertPEM: ca, ServiceCertPEM: cert, ServiceKeyPEM: key,
	}, nil
}

// List returns every pending node, oldest announcement first - the
// order a human triaging a queue would actually want.
func (s *Store) List() []*Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AnnouncedAt.Before(out[j].AnnouncedAt) })
	return out
}

func (s *Store) Get(id string) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	return n, ok
}

// Add records a new self-announcement, generating its ID. A node
// re-announcing with the same address as an already-pending entry
// still gets a separate new entry - de-duplication is a human decision
// at approval time (reject the stale one), not something this store
// enforces on its own.
func (s *Store) Add(node *Node) error {
	id, err := randomID()
	if err != nil {
		return fmt.Errorf("generate pending id: %w", err)
	}
	node.ID = id
	node.AnnouncedAt = time.Now()

	dir := filepath.Join(s.dir, "pending", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	metaBytes, err := json.Marshal(meta{ID: id, Name: node.Name, Address: node.Address, AnnouncedAt: node.AnnouncedAt})
	if err != nil {
		return fmt.Errorf("marshal meta.json: %w", err)
	}
	writes := []struct {
		name string
		data []byte
	}{
		{"meta.json", metaBytes},
		{"ca.crt", node.CACertPEM},
		{"service.crt", node.ServiceCertPEM},
		{"service.key", node.ServiceKeyPEM},
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(dir, w.name), w.data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", w.name, err)
		}
	}

	s.mu.Lock()
	s.nodes[id] = node
	s.mu.Unlock()
	return nil
}

// Remove discards a pending entry - used both by rejection and by
// approval, which reads the entry, hands its data to store.Add, and
// only then removes it from here (see dashboard/backend/main.go).
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	_, ok := s.nodes[id]
	delete(s.nodes, id)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such pending node %q", id)
	}
	return os.RemoveAll(filepath.Join(s.dir, "pending", id))
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
