package contentsource

import (
	"strings"
	"testing"
)

// TestParseConfig_StoreOverlapRefused: equal or nested (bucket, prefix)
// pairs and nested local directories across memory: and intake: specs
// are refused at load; disjoint ones load.
func TestParseConfig_StoreOverlapRefused(t *testing.T) {
	const s3mem = "{type: s3, bucket: kb, prefix: %s, endpoint: https://s3.example.net, region: garage, path_style: true}"
	mem := func(prefix string) string { return strings.Replace(s3mem, "%s", prefix, 1) }
	for _, tc := range []struct {
		name, yaml string
		overlap    bool
	}{
		{"intake nested in memory",
			"content: {type: local, path: /srv/kb, memory: " + mem("kb/memory/") + "}\nintake: " + mem("kb/memory/intake/") + "\n", true},
		{"intake beside memory",
			"content: {type: local, path: /srv/kb, memory: " + mem("kb/memory/") + "}\nintake: " + mem("kb/intake/") + "\n", false},
		{"intake on another endpoint",
			"content: {type: local, path: /srv/kb, memory: " + mem("kb/memory/") + "}\nintake: {type: s3, bucket: kb, prefix: kb/memory/intake/, endpoint: https://s3.other.net}\n", false},
		{"two collections on one prefix",
			"collections:\n  - name: a\n    type: local\n    path: /srv/a\n    memory: " + mem("kb/memory/") + "\n  - name: b\n    type: local\n    path: /srv/b\n    memory: " + mem("kb/memory") + "\n", true},
		{"whole bucket over a prefix",
			"collections:\n  - name: a\n    type: local\n    path: /srv/a\n    memory: " + mem("") + "\n  - name: b\n    type: local\n    path: /srv/b\n    memory: " + mem("kb/b/") + "\n", true},
		{"nested local intake",
			"content: {type: local, path: /srv/kb, memory: {type: local, path: /srv/mem}}\nintake: {type: local, path: /srv/mem/intake}\n", true},
		{"relative memory dirs under one local source",
			"collections:\n  - name: a\n    type: local\n    path: /srv/kb\n    memory: {type: local, path: mem}\n  - name: b\n    type: local\n    path: /srv/kb\n    memory: {type: local, path: mem/b}\n", true},
		{"default memory dirs under different sources",
			"collections:\n  - name: a\n    type: local\n    path: /srv/a\n    memory: {type: local}\n  - name: b\n    type: local\n    path: /srv/b\n    memory: {type: local}\n", false},
		{"sibling local dirs",
			"content: {type: local, path: /srv/kb, memory: {type: local, path: /srv/mem}}\nintake: {type: local, path: /srv/memory-intake}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.yaml), "/etc/meerkat/content-source.yaml")
			if !tc.overlap {
				if err != nil {
					t.Fatalf("disjoint stores refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatalf("err = %v, want an overlap refusal", err)
			}
		})
	}
}
