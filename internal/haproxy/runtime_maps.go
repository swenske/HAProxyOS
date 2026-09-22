package haproxy

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// Maps and ACLs are only visible/manageable through the runtime API when
// they're file-backed (e.g. `... map(/etc/haproxy/maps/x.map)` or
// `acl foo src -f /etc/haproxy/acls/x.acl` in the running config) -
// inline ACLs and maps with no backing file don't show up here at all.
// Verified against a real haproxy instance during development (see the
// commit that added this file): `show map`/`show acl` list entries as
// "<id> (<file>) <description>"; a bare "()" means no backing file, and
// those are skipped.
var identifierListLineRE = regexp.MustCompile(`^\d+\s+\(([^)]*)\)`)

func parseIdentifierList(out []byte) []string {
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := identifierListLineRE.FindStringSubmatch(line)
		if m == nil || m[1] == "" {
			continue
		}
		names = append(names, m[1])
	}
	return names
}

// MapList runs "show map" and returns every file-backed map's identifier
// (the same string used to address it in MapGet/MapUpdate).
func (m *Manager) MapList() ([]string, error) {
	out, err := m.statsCommand("show map")
	if err != nil {
		return nil, err
	}
	return parseIdentifierList(out), nil
}

// MapGet runs "show map <mapName>" and returns its key/value entries.
// Each line of output is "<internal-id> <key> <value...>" - the value is
// everything after the key, rejoined, since it may itself contain spaces.
func (m *Manager) MapGet(mapName string) (map[string]string, error) {
	out, err := m.statsCommand("show map " + mapName)
	if err != nil {
		return nil, err
	}

	entries := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		entries[fields[1]] = strings.Join(fields[2:], " ")
	}
	return entries, nil
}

// MapUpdate deletes or upserts a single map entry. HAProxy's "set map"
// only modifies an *existing* key (confirmed empirically: it returns
// "entry not found." on a missing key, it does not create one), so an
// upsert is implemented as delete-then-add instead - the delete step is
// allowed to fail (the key not existing yet is the normal case for a
// fresh entry, not an error).
func (m *Manager) MapUpdate(mapName, key, value string, del bool) error {
	if del {
		return mustEmpty(m.statsCommand(fmt.Sprintf("del map %s %s", mapName, key)))
	}
	_, _ = m.statsCommand(fmt.Sprintf("del map %s %s", mapName, key))
	return mustEmpty(m.statsCommand(fmt.Sprintf("add map %s %s %s", mapName, key, value)))
}

// ACLUpdate deletes or upserts a single ACL pattern value. Same
// delete-then-add upsert reasoning as MapUpdate - ACL entries are a flat
// value list (no key), and "add acl" would otherwise create a duplicate
// entry if the value is already present.
func (m *Manager) ACLUpdate(aclName, value string, del bool) error {
	if del {
		return mustEmpty(m.statsCommand(fmt.Sprintf("del acl %s %s", aclName, value)))
	}
	_, _ = m.statsCommand(fmt.Sprintf("del acl %s %s", aclName, value))
	return mustEmpty(m.statsCommand(fmt.Sprintf("add acl %s %s", aclName, value)))
}

// mustEmpty treats a non-empty (after trimming) response from a runtime
// API mutation command as an error - empirically, HAProxy's stats socket
// returns an empty line on success and a human-readable message
// ("Key not found.", "entry not found.", ...) on failure for
// add/set/del map and add/del acl.
func mustEmpty(out []byte, err error) error {
	if err != nil {
		return err
	}
	if trimmed := bytes.TrimSpace(out); len(trimmed) > 0 {
		return fmt.Errorf("%s", trimmed)
	}
	return nil
}
