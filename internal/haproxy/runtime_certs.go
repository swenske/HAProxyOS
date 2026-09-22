package haproxy

import (
	"fmt"
	"strings"
	"time"
)

// notAfterLayout matches HAProxy's own "show ssl cert <name>" output
// format for the notAfter line (OpenSSL ASN1_TIME print style, verified
// empirically against a real cert: "Sep 23 16:12:14 2026 GMT" - note the
// space-padded (not zero-padded) day of month).
const notAfterLayout = "Jan _2 15:04:05 2006 MST"

// CertInfo is one entry from CertificateList.
type CertInfo struct {
	Name     string
	NotAfter string // RFC3339, or the raw HAProxy string if it didn't parse
}

// CertificateList runs "show ssl cert" for the name list, then
// "show ssl cert <name>" on each to pull its notAfter date.
func (m *Manager) CertificateList() ([]CertInfo, error) {
	out, err := m.statsCommand("show ssl cert")
	if err != nil {
		return nil, err
	}

	var certs []CertInfo
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}

		detail, err := m.statsCommand("show ssl cert " + name)
		if err != nil {
			return nil, err
		}
		certs = append(certs, CertInfo{Name: name, NotAfter: parseNotAfter(detail)})
	}
	return certs, nil
}

func parseNotAfter(detail []byte) string {
	for _, line := range strings.Split(string(detail), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != "notAfter" {
			continue
		}
		value = strings.TrimSpace(value)
		t, err := time.Parse(notAfterLayout, value)
		if err != nil {
			return value // best effort - surface the raw string rather than dropping it
		}
		return t.Format(time.RFC3339)
	}
	return ""
}

// CertificateUpload creates (or, if name already exists, reuses) a
// certificate store entry, stages pemBundle into it, and commits it.
// This manages HAProxy's in-memory certificate store - it does NOT by
// itself bind the certificate to any listener (that still happens
// through a `bind ... ssl crt-list <list>` in the config applied via
// HAProxyService.ApplyConfig, with `add ssl crt-list` wiring this store
// entry in - not implemented yet, see docs/architecture.md). A freshly
// uploaded certificate is loaded and inspectable (CertificateList) but
// reports "Unused" until something references it.
//
// "set ssl cert" was found empirically to give no reliable success/
// failure signal of its own (empty response either way in testing) - the
// subsequent "commit" is the authoritative result, so that's what's
// checked here.
func (m *Manager) CertificateUpload(name string, pemBundle []byte) error {
	// Ignore the result: "already exists" just means we're updating a
	// cert that's already in the store, which is fine.
	_, _ = m.statsCommand("new ssl cert " + name)

	if _, err := m.statsCommand("set ssl cert " + name + " <<\n" + string(pemBundle)); err != nil {
		return fmt.Errorf("stage certificate: %w", err)
	}

	commitOut, err := m.statsCommand("commit ssl cert " + name)
	if err != nil {
		return err
	}
	if !strings.Contains(string(commitOut), "Success!") {
		_, _ = m.statsCommand("abort ssl cert " + name)
		return fmt.Errorf("commit certificate %s: %s", name, strings.TrimSpace(string(commitOut)))
	}
	return nil
}

// CertificateDelete removes a certificate store entry. Fails if the
// certificate is still referenced by a live crt-list/bind (HAProxy
// itself refuses that, see the "doesn't exist"/in-use messages below).
func (m *Manager) CertificateDelete(name string) error {
	out, err := m.statsCommand("del ssl cert " + name)
	if err != nil {
		return err
	}
	msg := string(out)
	if strings.Contains(msg, "doesn't exist") {
		return fmt.Errorf("certificate %s not found", name)
	}
	if !strings.Contains(msg, "deleted") {
		return fmt.Errorf("delete certificate %s: %s", name, strings.TrimSpace(msg))
	}
	return nil
}
