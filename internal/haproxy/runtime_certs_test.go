package haproxy

import "testing"

// Fixture captured verbatim from `show ssl cert <name>` against a real
// haproxy 3.4.0 instance during development.
const showSSLCertFixture = `Filename: /tmp/newcert.pem
Option: ocsp-update off
Option: jwt off
Status: Unused
Serial: 3A83DFCAD09C3E78CD85094EFF488F11EF10CF3B
notBefore: Sep 22 16:12:14 2026 GMT
notAfter: Sep 23 16:12:14 2026 GMT
Subject Alternative Name:
Algorithm: OCSP Response Key:
`

func TestParseNotAfter(t *testing.T) {
	got := parseNotAfter([]byte(showSSLCertFixture))
	want := "2026-09-23T16:12:14Z"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseNotAfter_SingleDigitDay(t *testing.T) {
	// OpenSSL's ASN1_TIME print format space-pads (not zero-pads) the
	// day of month - "Sep  3" not "Sep 03" - notAfterLayout's "_2" must
	// handle that.
	got := parseNotAfter([]byte("notAfter: Sep  3 01:02:03 2027 GMT\n"))
	want := "2027-09-03T01:02:03Z"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseNotAfter_Missing(t *testing.T) {
	if got := parseNotAfter([]byte("Filename: x\nStatus: Unused\n")); got != "" {
		t.Fatalf("expected empty string when notAfter is absent, got %q", got)
	}
}
