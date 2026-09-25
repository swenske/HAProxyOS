package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/pending"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// startRegistrationListener serves the node self-registration endpoint
// on its own dedicated TLS port - deliberately separate from :8080
// (plain HTTP, human-facing, now password-gated - see internal/auth)
// and the per-node listener pool (9500-9599, mTLS-gated per *approved*
// node): a self-announcing node doesn't have an approved identity yet,
// so there's nothing to gate this behind except "is this really the
// Controller", which the node itself checks via the same CA cert it
// was given at provisioning time (see internal/pending's own doc
// comment, and the node-side half of this design, not yet built).
// tls.NoClientCert on purpose: the whole point of this endpoint is to
// accept an announcement from a node the Controller has never seen
// before, so there's no client certificate to require yet.
func (a *app) startRegistrationListener(addr string) error {
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{a.serverCert},
		ClientAuth:   tls.NoClientCert,
		MinVersion:   tls.VersionTLS13,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/register", a.handleRegister)

	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: mux}
	go func() {
		// ErrServerClosed is the expected outcome of a graceful shutdown,
		// not a real failure - dashboardd has no such shutdown path today
		// (it just runs until killed), but the check costs nothing and
		// matches nodeproxy.Listener's own pattern.
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("registration listener: %v", err)
		}
	}()
	log.Printf("dashboardd registration endpoint listening on %s", addr)
	return nil
}

// registerRequest is what a self-registering node posts. ServiceCertPEM/
// ServiceKeyPEM are already the credential the node generated for
// itself - see internal/pending's own doc comment for why this handler
// doesn't need to exchange a bootstrap credential for anything, unlike
// the human-driven add-node flow.
type registerRequest struct {
	Name           string `json:"name"`
	Address        string `json:"address"`
	CACertPEM      string `json:"ca_cert_pem"`
	ServiceCertPEM string `json:"service_cert_pem"`
	ServiceKeyPEM  string `json:"service_key_pem"`
}

func (a *app) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Address == "" || req.CACertPEM == "" || req.ServiceCertPEM == "" || req.ServiceKeyPEM == "" {
		http.Error(w, "name, address, ca_cert_pem, service_cert_pem, and service_key_pem are all required", http.StatusBadRequest)
		return
	}
	// Catch an obviously malformed cert/key pair now, with a real
	// error, rather than only failing later when an operator approves
	// it and the per-node listener fails to start.
	if _, err := tls.X509KeyPair([]byte(req.ServiceCertPEM), []byte(req.ServiceKeyPEM)); err != nil {
		http.Error(w, "service_cert_pem/service_key_pem don't form a valid certificate: "+err.Error(), http.StatusBadRequest)
		return
	}

	node := &pending.Node{
		Name:           req.Name,
		Address:        req.Address,
		CACertPEM:      []byte(req.CACertPEM),
		ServiceCertPEM: []byte(req.ServiceCertPEM),
		ServiceKeyPEM:  []byte(req.ServiceKeyPEM),
	}
	if err := a.pending.Add(node); err != nil {
		http.Error(w, "record registration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("node self-registered: %s (%s), awaiting approval", node.Name, node.Address)
	writeJSON(w, http.StatusCreated, struct {
		ID string `json:"id"`
	}{ID: node.ID})
}

// handlePendingList is a read-only GET /api/pending, auth-gated like
// everything else under /api/.
func (a *app) handlePendingList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type pendingView struct {
		ID          string    `json:"id"`
		Name        string    `json:"name"`
		Address     string    `json:"address"`
		AnnouncedAt time.Time `json:"announced_at"`
	}
	var out []pendingView
	for _, n := range a.pending.List() {
		out = append(out, pendingView{ID: n.ID, Name: n.Name, Address: n.Address, AnnouncedAt: n.AnnouncedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePendingAction dispatches POST /api/pending/{id}/approve and
// POST /api/pending/{id}/reject - the two actions a human takes on the
// "pending" queue, both auth-gated like everything under /api/.
func (a *app) handlePendingAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/pending/")
	id, action, ok := strings.Cut(rest, "/")
	if !ok || id == "" || action == "" {
		http.Error(w, "usage: POST /api/pending/{id}/approve or /reject", http.StatusBadRequest)
		return
	}

	switch action {
	case "approve":
		a.approvePending(w, id)
	case "reject":
		a.rejectPending(w, id)
	default:
		http.Error(w, fmt.Sprintf("unknown action %q (want approve or reject)", action), http.StatusBadRequest)
	}
}

// approvePending moves a pending entry into the real node registry and
// starts its listener - the credential itself was already generated by
// the node at announcement time (see internal/pending's own doc
// comment), so unlike the human-driven add-node flow there's no
// GenerateClientConfiguration round trip here: the data just moves from
// one store to the other.
func (a *app) approvePending(w http.ResponseWriter, id string) {
	p, ok := a.pending.Get(id)
	if !ok {
		http.Error(w, fmt.Sprintf("no pending node %q", id), http.StatusNotFound)
		return
	}

	port, err := a.allocatePort()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
		return
	}

	node := &store.Node{
		Name:           p.Name,
		Address:        p.Address,
		Port:           port,
		CACertPEM:      p.CACertPEM,
		ServiceCertPEM: p.ServiceCertPEM,
		ServiceKeyPEM:  p.ServiceKeyPEM,
	}
	if err := a.store.Add(node); err != nil {
		http.Error(w, fmt.Sprintf("persist node: %v", err), http.StatusInternalServerError)
		return
	}
	if err := a.startListener(node); err != nil {
		// Roll the store entry back rather than leaving it orphaned: the
		// pending entry below stays in the queue specifically so this can
		// be retried (e.g. after freeing up a conflicting port), and a
		// retry calls store.Add again with a freshly allocated port - if
		// the failed attempt's row were left behind, retrying would just
		// pile up a permanently dead duplicate next to the working one.
		if removeErr := a.store.Remove(node.ID); removeErr != nil {
			log.Printf("node %s: failed to roll back after listener start failure: %v", node.ID, removeErr)
		}
		http.Error(w, fmt.Sprintf("node approved but its listener failed to start: %v", err), http.StatusInternalServerError)
		return
	}
	// Only remove the pending entry once the node is genuinely live -
	// a failed approval above leaves it in the queue so it can be
	// retried, rather than silently losing the announcement.
	if err := a.pending.Remove(id); err != nil {
		log.Printf("pending node %s: approved but failed to remove from the pending queue: %v", id, err)
	}
	log.Printf("node approved: %s (%s) -> port %d", node.Name, node.Address, node.Port)
	writeJSON(w, http.StatusCreated, struct {
		ID   string `json:"id"`
		Port int    `json:"port"`
	}{ID: node.ID, Port: node.Port})
}

// rejectPending just discards the announcement - the node itself isn't
// notified (there's no channel to notify it over; it'll either be
// re-provisioned with a corrected controller address/CA, or a human
// investigates why it showed up at all).
func (a *app) rejectPending(w http.ResponseWriter, id string) {
	if err := a.pending.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("pending node %s rejected", id)
	w.WriteHeader(http.StatusNoContent)
}
