package pki

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

// CertPool returns an x509.CertPool containing just this CA - used both
// to verify client certificates (server side) and to verify the server's
// own certificate (client side, since it's signed by the same CA).
func (ca *CA) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return pool
}

// ServerTLSConfig builds the tls.Config for janusd's gRPC listener:
// present serverCert, and require + verify a client certificate signed
// by this CA on every connection. There is no unauthenticated RPC - see
// docs/architecture.md.
func (ca *CA) ServerTLSConfig(serverCert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.CertPool(),
		MinVersion:   tls.VersionTLS13,
	}
}

// ClientTLSConfig builds a tls.Config for janusctl (or any external
// caller) from PEM-encoded material only - no access to the CA's private
// key, just its certificate (to verify the server) and a previously
// issued client certificate/key pair (to authenticate as).
func ClientTLSConfig(caCertPEM, clientCertPEM, clientKeyPEM []byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("no valid certificates found in CA PEM")
	}

	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load client certificate/key: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
