package selfregister

import (
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/swenske/Janus/internal/pki"
)

func TestReadMissingConfig(t *testing.T) {
	cfg, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cfg != nil {
		t.Fatalf("Read on an empty dir = %+v, want nil (no Controller provisioned)", cfg)
	}
}

func TestReadEmptyAddress(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, addressFile), []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("Read with a blank (whitespace-only) address file should error, got nil")
	}
}

func TestReadValid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, addressFile), []byte("10.0.0.5:8443\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, caFile), []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cfg.Address != "10.0.0.5:8443" {
		t.Errorf("Address = %q, want %q (trailing newline trimmed)", cfg.Address, "10.0.0.5:8443")
	}
	if len(cfg.CACertPEM) == 0 {
		t.Error("CACertPEM is empty")
	}
}

func TestAlreadyRegisteredMarkRegistered(t *testing.T) {
	dir := t.TempDir()
	if AlreadyRegistered(dir) {
		t.Fatal("AlreadyRegistered on a fresh dir = true, want false")
	}
	if err := MarkRegistered(dir); err != nil {
		t.Fatalf("MarkRegistered: %v", err)
	}
	if !AlreadyRegistered(dir) {
		t.Fatal("AlreadyRegistered after MarkRegistered = false, want true")
	}
}

func TestRegisterSuccess(t *testing.T) {
	var gotReq registerRequest
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/register" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	ca, err := pki.NewCA("test node CA")
	if err != nil {
		t.Fatalf("pki.NewCA: %v", err)
	}

	// httptest.NewTLSServer's own certificate is self-signed, so it is
	// its own trust root - exactly the shape a real node's controller/
	// ca.crt would take (the Controller's own CA/identity cert).
	cfg := &Config{Address: srv.Listener.Addr().String(), CACertPEM: certPEM(t, srv)}

	if err := Register(cfg, ca, "test-node", "10.1.2.3:9505"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if gotReq.Name != "test-node" {
		t.Errorf("Name = %q, want %q", gotReq.Name, "test-node")
	}
	if gotReq.Address != "10.1.2.3:9505" {
		t.Errorf("Address = %q, want %q", gotReq.Address, "10.1.2.3:9505")
	}
	if gotReq.CACertPEM != string(ca.CertPEM) {
		t.Error("CACertPEM in the request doesn't match the node's own CA cert")
	}
	// The service credential must be freshly minted (never the node's
	// own root CA key/cert sent as-is) and must actually parse as a
	// valid cert/key pair.
	if gotReq.ServiceCertPEM == string(ca.CertPEM) {
		t.Error("ServiceCertPEM equals the node's own root CA certificate - must be a freshly-issued leaf, never the root")
	}
	if _, err := tls.X509KeyPair([]byte(gotReq.ServiceCertPEM), []byte(gotReq.ServiceKeyPEM)); err != nil {
		t.Errorf("service credential doesn't form a valid cert/key pair: %v", err)
	}
}

func TestRegisterWrongCARefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	ca, err := pki.NewCA("test node CA")
	if err != nil {
		t.Fatalf("pki.NewCA: %v", err)
	}
	// An unrelated CA, not the one that actually signed srv's
	// certificate - the handshake must fail, proving Register verifies
	// the Controller's identity against cfg.CACertPEM specifically and
	// not, say, an ambient trust store.
	wrongCA, err := pki.NewCA("unrelated CA")
	if err != nil {
		t.Fatalf("pki.NewCA: %v", err)
	}

	cfg := &Config{Address: srv.Listener.Addr().String(), CACertPEM: wrongCA.CertPEM}
	if err := Register(cfg, ca, "test-node", "10.1.2.3:9505"); err == nil {
		t.Fatal("Register against a server whose cert isn't signed by the given CA succeeded, want a TLS verification failure")
	}
}

func TestRegisterControllerRejects(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no thanks", http.StatusBadRequest)
	}))
	defer srv.Close()

	ca, err := pki.NewCA("test node CA")
	if err != nil {
		t.Fatalf("pki.NewCA: %v", err)
	}
	cfg := &Config{Address: srv.Listener.Addr().String(), CACertPEM: certPEM(t, srv)}
	err = Register(cfg, ca, "test-node", "10.1.2.3:9505")
	if err == nil {
		t.Fatal("Register against a Controller that refuses the request succeeded, want an error")
	}
}

func TestDetectAdvertiseAddress(t *testing.T) {
	addr, err := DetectAdvertiseAddress("127.0.0.1:12345", ":9505")
	if err != nil {
		t.Fatalf("DetectAdvertiseAddress: %v", err)
	}
	if addr != "127.0.0.1:9505" {
		t.Errorf("DetectAdvertiseAddress = %q, want %q", addr, "127.0.0.1:9505")
	}
}

func TestDetectAdvertiseAddressBadGRPCAddr(t *testing.T) {
	if _, err := DetectAdvertiseAddress("127.0.0.1:12345", "not-a-valid-addr"); err == nil {
		t.Fatal("DetectAdvertiseAddress with an unparseable grpcAddr succeeded, want an error")
	}
}

// certPEM extracts srv's own leaf certificate as PEM - httptest.Server's
// certificate is self-signed, so this is exactly what a real Controller
// CA cert distributed at provisioning time would look like.
func certPEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	if len(srv.Certificate().Raw) == 0 {
		t.Fatal("test server has no certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}
