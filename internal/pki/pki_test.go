package pki

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	ca, err := NewCA("test CA")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	certPEM, _, err := ca.Issue(IssueOptions{CommonName: "leaf", Roles: []string{RoleAdmin}})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	block, _ := pemDecode(t, certPEM)
	leaf, err := x509.ParseCertificate(block)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{Roots: ca.CertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatalf("leaf cert did not verify against its own CA: %v", err)
	}

	if got := leaf.Subject.Organization; len(got) != 1 || got[0] != RoleAdmin {
		t.Fatalf("roles not carried in Subject.Organization: got %v", got)
	}
}

func TestLoadCARoundTrip(t *testing.T) {
	ca, err := NewCA("test CA")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatalf("KeyPEM: %v", err)
	}

	loaded, err := LoadCA(ca.CertPEM, keyPEM)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}

	// A cert issued by the reloaded CA must still verify against the
	// original CA's pool - proves the key round-tripped correctly, not
	// just the certificate.
	certPEM, _, err := loaded.Issue(IssueOptions{CommonName: "leaf"})
	if err != nil {
		t.Fatalf("Issue after reload: %v", err)
	}
	block, _ := pemDecode(t, certPEM)
	leaf, err := x509.ParseCertificate(block)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: ca.CertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatalf("cert issued by reloaded CA did not verify against original CA pool: %v", err)
	}
}

func TestMTLSHandshake(t *testing.T) {
	ca, err := NewCA("test CA")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	serverCertPEM, serverKeyPEM, err := ca.Issue(IssueOptions{
		CommonName:  "server",
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		t.Fatalf("issue server cert: %v", err)
	}
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	clientCertPEM, clientKeyPEM, err := ca.Issue(IssueOptions{
		CommonName:  "client",
		Roles:       []string{RoleAdmin},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		t.Fatalf("issue client cert: %v", err)
	}

	lis, err := tls.Listen("tcp", "127.0.0.1:0", ca.ServerTLSConfig(serverCert))
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer lis.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	t.Run("valid client cert succeeds", func(t *testing.T) {
		clientTLSConfig, err := ClientTLSConfig(ca.CertPEM, clientCertPEM, clientKeyPEM)
		if err != nil {
			t.Fatalf("ClientTLSConfig: %v", err)
		}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", lis.Addr().String(), clientTLSConfig)
		if err != nil {
			t.Fatalf("dial with valid client cert: %v", err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := <-serverErr; err != nil {
			t.Fatalf("server side: %v", err)
		}
	})

	t.Run("no client cert is rejected", func(t *testing.T) {
		go func() {
			// Second connection for this subtest - server just needs to
			// attempt an accept; the handshake itself is expected to fail
			// before any bytes are read.
			conn, err := lis.Accept()
			if err == nil {
				conn.Close()
			}
		}()

		otherCA, err := NewCA("unrelated CA")
		if err != nil {
			t.Fatalf("NewCA: %v", err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(otherCA.Cert)

		//nolint:gosec // deliberately verifying the handshake is rejected, not making a trusted connection
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", lis.Addr().String(), &tls.Config{
			RootCAs:    ca.CertPool(),
			MinVersion: tls.VersionTLS13,
		})
		if err == nil {
			conn.Close()
			t.Fatalf("expected handshake without a client certificate to be rejected, it succeeded")
		}
	})
}

func pemDecode(t *testing.T, data []byte) ([]byte, []byte) {
	t.Helper()
	block, rest := pem.Decode(data)
	if block == nil {
		t.Fatalf("no PEM block found")
	}
	return block.Bytes, rest
}
