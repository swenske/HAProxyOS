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
	Status   string // "Used" or "Unused", straight from HAProxy
}

// CertificateList runs "show ssl cert" for the name list, then
// "show ssl cert <name>" on each to pull its notAfter date and status.
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
		certs = append(certs, CertInfo{Name: name, NotAfter: parseNotAfter(detail), Status: parseCertField(detail, "Status")})
	}
	return certs, nil
}

func parseNotAfter(detail []byte) string {
	value := parseCertField(detail, "notAfter")
	if value == "" {
		return ""
	}
	t, err := time.Parse(notAfterLayout, value)
	if err != nil {
		return value // best effort - surface the raw string rather than dropping it
	}
	return t.Format(time.RFC3339)
}

func parseCertField(detail []byte, field string) string {
	for _, line := range strings.Split(string(detail), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != field {
			continue
		}
		return strings.TrimSpace(value)
	}
	return ""
}

// CertificateUpload creates (or, if name already exists, reuses) a
// certificate store entry, stages pemBundle into it, and commits it. If
// crtList is non-empty, also binds it into that crt-list (a `bind ...
// ssl crt-list <path>` file already referenced by the running config),
// optionally scoped to sni - the SNI names HAProxy will match against to
// pick this certificate, same as a manually-authored crt-list line.
// Binding is what actually makes an uploaded certificate reachable by
// TLS clients; without it, the certificate is loaded and inspectable
// (CertificateList) but never served (reports "Unused").
//
// "set ssl cert" was found empirically to give no reliable success/
// failure signal of its own (empty response either way in testing) - the
// subsequent "commit" is the authoritative result, so that's what's
// checked here.
func (m *Manager) CertificateUpload(name string, pemBundle []byte, crtList string, sni []string) error {
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

	if crtList == "" {
		return nil
	}

	addCmd := "add ssl crt-list " + crtList + " " + name
	if len(sni) > 0 {
		addCmd += " " + strings.Join(sni, " ")
	}
	addOut, err := m.statsCommand(addCmd)
	if err != nil {
		return fmt.Errorf("bind certificate %s to crt-list %s: %w", name, crtList, err)
	}
	if !strings.Contains(string(addOut), "Success!") {
		return fmt.Errorf("bind certificate %s to crt-list %s: %s", name, crtList, strings.TrimSpace(string(addOut)))
	}
	return nil
}

// CertificateDelete removes a certificate store entry. If crtList is
// non-empty, unbinds it from that crt-list first - HAProxy refuses to
// delete a certificate still bound to any crt-list ("in use, can't be
// deleted!"), so a bound certificate's crtList must be given or the
// delete below fails with that message. The unbind step's own result
// isn't checked: if it genuinely didn't work, the delete that follows
// will fail with a clear message of its own; if the certificate simply
// wasn't in that crt-list to begin with, unbinding is a harmless no-op,
// same reasoning as the delete-then-add upsert in runtime_maps.go.
func (m *Manager) CertificateDelete(name, crtList string) error {
	if crtList != "" {
		_, _ = m.statsCommand("del ssl crt-list " + crtList + " " + name)
	}

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
