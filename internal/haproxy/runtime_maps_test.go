package haproxy

import (
	"reflect"
	"testing"
)

// Fixtures below are captured verbatim from a real haproxy 3.4.0 instance
// during development (see the commit that added this file) - not
// hand-guessed formats.

func TestParseIdentifierList(t *testing.T) {
	out := []byte("# id (file) description\n" +
		"0 (/tmp/test-maps/hosts.map) pattern loaded from file '/tmp/test-maps/hosts.map' used by map at file '/tmp/test.cfg' line 12. curr_ver=0 next_ver=0 entry_cnt=1\n" +
		"1 () acl 'var' file 'httpclient' line 0. curr_ver=0 next_ver=0 entry_cnt=1\n" +
		"\n")

	got := parseIdentifierList(out)
	want := []string{"/tmp/test-maps/hosts.map"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (non-file-backed entries like \"1 ()\" must be skipped)", got, want)
	}
}

func TestParseIdentifierList_Empty(t *testing.T) {
	got := parseIdentifierList([]byte("# id (file) description\n\n"))
	if len(got) != 0 {
		t.Fatalf("expected no identifiers, got %v", got)
	}
}
