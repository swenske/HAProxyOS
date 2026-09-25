package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/pending"
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
// everything else under /api/ - approve/reject actions land in a later
// tranche alongside the UI that needs them; this exists now so the
// registration flow above is actually verifiable end to end.
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
