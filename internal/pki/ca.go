// Package pki is Janus's minimal internal certificate authority: one
// self-signed CA per node, used to issue the node's own gRPC server
// certificate and short-lived client certificates (see
// SystemService.GenerateClientConfiguration). Every certificate is
// ECDSA P-256 - was originally Ed25519 (small keys, fast, no
// padding-oracle history), switched after a real Proxmox deployment hit
// a browser dead end: the dashboard's own per-node view needs a real
// browser to both verify the server certificate and sign a TLS client
// certificate presented for mutual TLS (see dashboard/backend/internal/
// nodeproxy), and Chromium's TLS stack (Chrome/Brave/Edge) has a long,
// well-documented history of not supporting Ed25519 at all - general
// server-certificate support only landed around Chrome 137 (May 2025),
// and reliable client-certificate-authentication support is still far
// from guaranteed even now. P-256 has no such gap in any mainstream TLS
// stack, browser or otherwise - confirmed empirically (ERR_SSL_
// VERSION_OR_CIPHER_MISMATCH from a real Brave browser against the old
// Ed25519 dashboard identity cert, before this switch).
//
// Roles are carried in the leaf certificate's Subject.Organization field
// (the same idiom Kubernetes client-cert auth uses for group membership)
// so a future authorization layer can read them straight off the verified
// peer certificate without a custom X.509 extension. No RPC actually
// checks roles yet - see docs/architecture.md's Phase 2 notes: this
// package covers authentication (proving who you are), not yet
// authorization (what that identity is allowed to do).
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

const (
	caKeyPEMType   = "PRIVATE KEY"
	caCertPEMType  = "CERTIFICATE"
	caValidity     = 10 * 365 * 24 * time.Hour // 10 years - this is the trust root, not meant to rotate casually
	leafValidity   = 365 * 24 * time.Hour      // 1 year - no rotation/renewal flow yet (Phase 2+ gap, see docs)
	serialBitsSize = 128
)

// CA is a self-signed certificate authority and the private key that
// backs it.
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	Key     *ecdsa.PrivateKey
}

// NewCA generates a brand new, self-signed CA.
func NewCA(commonName string) (*CA, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	pub := &priv.PublicKey

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour), // small clock-skew margin
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	return &CA{Cert: cert, CertPEM: encodePEM(caCertPEMType, der), Key: priv}, nil
}

// LoadCA parses a CA from its PEM-encoded certificate and private key
// (as previously written by CA.KeyPEM/CertPEM).
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("decode CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("decode CA key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("CA key is not ECDSA")
	}

	return &CA{Cert: cert, CertPEM: certPEM, Key: ecKey}, nil
}

// KeyPEM returns the CA's private key, PKCS#8/PEM-encoded.
func (ca *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(ca.Key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	return encodePEM(caKeyPEMType, der), nil
}

// IssueOptions describes the leaf certificate CA.Issue should produce.
type IssueOptions struct {
	CommonName string
	// Roles is carried in the certificate's Subject.Organization - see
	// the package doc comment.
	Roles []string
	// DNSNames/IPAddresses are only meaningful for server certificates
	// (ExtKeyUsageServerAuth) - a pure client cert needs neither.
	DNSNames    []string
	IPAddresses []net.IP
	ExtKeyUsage []x509.ExtKeyUsage
}

// Issue signs a new ECDSA P-256 leaf certificate with the CA's key,
// valid for leafValidity from now. Returns (certPEM, keyPEM).
func (ca *CA) Issue(opts IssueOptions) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf key: %w", err)
	}
	pub := &priv.PublicKey

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   opts.CommonName,
			Organization: opts.Roles,
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(leafValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: opts.ExtKeyUsage,
		DNSNames:    opts.DNSNames,
		IPAddresses: opts.IPAddresses,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("create leaf certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal leaf key: %w", err)
	}

	return encodePEM(caCertPEMType, der), encodePEM(caKeyPEMType, keyDER), nil
}

func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), serialBitsSize)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

func encodePEM(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}
