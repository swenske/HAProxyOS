package pki

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

const (
	caCertFile     = "ca.crt"
	caKeyFile      = "ca.key"
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"
	adminCertFile  = "admin.crt"
	adminKeyFile   = "admin.key"

	// RoleAdmin is the only role that exists so far - see the package
	// doc comment: nothing enforces it yet, it's just carried on the cert.
	RoleAdmin = "os:admin"
)

// Bootstrap is the result of loading (or, on first boot, generating) a
// node's PKI material.
type Bootstrap struct {
	CA         *CA
	ServerCert tls.Certificate

	// AdminIssued is true only when the admin client certificate was
	// freshly generated this run (first boot ever, or the pki dir was
	// wiped) - the caller should surface AdminCertPEM/AdminKeyPEM once,
	// since after this they're only readable from disk (no shell to
	// retrieve them later on the real target OS - see the "no shell"
	// design goal in docs/architecture.md). This is the node's bootstrap
	// trust anchor until Phase 3's Install flow hands out credentials
	// through a proper side channel.
	AdminIssued  bool
	AdminCertPEM []byte
	AdminKeyPEM  []byte
}

// LoadOrBootstrap loads an existing PKI from dir, or generates a brand
// new CA + server certificate (for hostname/extra SANs) + initial admin
// client certificate if dir is empty.
func LoadOrBootstrap(dir, hostname string, extraIPs []net.IP) (*Bootstrap, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	caCertPath := filepath.Join(dir, caCertFile)
	if _, err := os.Stat(caCertPath); err == nil {
		return load(dir)
	}

	return bootstrap(dir, hostname, extraIPs)
}

func load(dir string) (*Bootstrap, error) {
	caCertPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", caCertFile, err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", caKeyFile, err)
	}
	ca, err := LoadCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}

	serverCert, err := tls.LoadX509KeyPair(filepath.Join(dir, serverCertFile), filepath.Join(dir, serverKeyFile))
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}

	return &Bootstrap{CA: ca, ServerCert: serverCert}, nil
}

func bootstrap(dir, hostname string, extraIPs []net.IP) (*Bootstrap, error) {
	ca, err := NewCA("HAProxyOS node CA: " + hostname)
	if err != nil {
		return nil, fmt.Errorf("generate CA: %w", err)
	}
	caKeyPEM, err := ca.KeyPEM()
	if err != nil {
		return nil, err
	}
	if err := writeFiles(dir,
		file{caCertFile, ca.CertPEM, 0o644},
		file{caKeyFile, caKeyPEM, 0o600},
	); err != nil {
		return nil, err
	}

	ips := append([]net.IP{net.ParseIP("127.0.0.1")}, extraIPs...)
	serverCertPEM, serverKeyPEM, err := ca.Issue(IssueOptions{
		CommonName:  hostname,
		DNSNames:    []string{hostname, "localhost"},
		IPAddresses: ips,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("issue server certificate: %w", err)
	}
	if err := writeFiles(dir,
		file{serverCertFile, serverCertPEM, 0o644},
		file{serverKeyFile, serverKeyPEM, 0o600},
	); err != nil {
		return nil, err
	}

	adminCertPEM, adminKeyPEM, err := ca.Issue(IssueOptions{
		CommonName:  "admin",
		Roles:       []string{RoleAdmin},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("issue admin certificate: %w", err)
	}
	if err := writeFiles(dir,
		file{adminCertFile, adminCertPEM, 0o600},
		file{adminKeyFile, adminKeyPEM, 0o600},
	); err != nil {
		return nil, err
	}

	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load freshly-issued server certificate: %w", err)
	}

	return &Bootstrap{
		CA:           ca,
		ServerCert:   serverCert,
		AdminIssued:  true,
		AdminCertPEM: adminCertPEM,
		AdminKeyPEM:  adminKeyPEM,
	}, nil
}

type file struct {
	name string
	data []byte
	mode os.FileMode
}

func writeFiles(dir string, files ...file) error {
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
}
