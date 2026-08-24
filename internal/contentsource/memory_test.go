package contentsource

import (
	"context"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/memory"
)

// memory_test.go covers the `memory:` block: that it parses, that it is
// validated at LOAD time (so a mistake fails the process rather than
// the first save), and that it survives resolution into the resolved
// collection a registry is built from.

func TestParseConfig_MemoryBlock(t *testing.T) {
	cfg, err := parseConfig([]byte(`
collections:
  - name: notes
    type: local
    path: ../notes
    memory:
      type: local
      path: /srv/meerkat/notes-memory
  - name: shared
    type: gcs
    bucket: my-org-knowledge
    prefix: kb/live/
    memory:
      type: gcs
      bucket: my-org-knowledge
      prefix: kb/memory/
  - name: archive
    type: local
    path: ../archive
`), "content-source.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if got := cfg.Collections[0].Memory; got == nil || got.Type != memory.BackendLocal || got.Path != "/srv/meerkat/notes-memory" {
		t.Errorf("notes memory = %+v", got)
	}
	if got := cfg.Collections[1].Memory; got == nil || got.Type != memory.BackendGCS || got.Bucket != "my-org-knowledge" {
		t.Errorf("shared memory = %+v", got)
	}
	// No memory: block means a read-only collection — which is what
	// every configuration that predates this feature has.
	if cfg.Collections[2].Memory != nil {
		t.Errorf("archive picked up a memory store it never declared: %+v", cfg.Collections[2].Memory)
	}
}

func TestParseConfig_MemoryBlockOnASingleSource(t *testing.T) {
	cfg, err := parseConfig([]byte("content:\n  type: local\n  path: kb\n  memory:\n    type: local\n"), "content-source.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Content.Memory == nil || cfg.Content.Memory.Type != memory.BackendLocal {
		t.Fatalf("content memory = %+v", cfg.Content.Memory)
	}
}

// TestParseConfig_PersonalVisibility covers the read-policy key: that
// it parses, that omitting it is the SECURE answer rather than a
// missing one, and that an unrecognised value fails the process at load
// time instead of being ignored.
func TestParseConfig_PersonalVisibility(t *testing.T) {
	cfg, err := parseConfig([]byte(`
collections:
  - name: notes
    type: local
    path: ../notes
    memory:
      type: local
      path: /srv/notes-memory
      personal_visibility: collection
  - name: shared
    type: local
    path: ../shared
    memory:
      type: local
      path: /srv/shared-memory
      personal_visibility: private
  - name: quiet
    type: local
    path: ../quiet
    memory:
      type: local
      path: /srv/quiet-memory
  - name: archive
    type: local
    path: ../archive
`), "content-source.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	for i, want := range []string{
		memory.VisibilityCollection,
		memory.VisibilityPrivate,
		// Omitted: private, so a memory: block written before the key
		// existed keeps personal memories private without being edited.
		memory.VisibilityPrivate,
		// No memory: block at all: private too.
		memory.VisibilityPrivate,
	} {
		if got := cfg.Collections[i].Memory.Visibility(); got != want {
			t.Errorf("collections[%d] (%s) visibility = %q, want %q",
				i, cfg.Collections[i].Name, got, want)
		}
	}
}

func TestParseConfig_MemoryValidationRunsAtLoadTime(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"unknown personal_visibility": {
			"content:\n  type: local\n  path: kb\n  memory:\n    type: local\n    personal_visibility: public\n",
			"personal_visibility must be private or collection",
		},
		"no type": {
			"content:\n  type: local\n  path: kb\n  memory:\n    path: m\n",
			"memory.type is required",
		},
		"unknown type": {
			"content:\n  type: local\n  path: kb\n  memory:\n    type: s3\n    path: m\n",
			"must be local or gcs",
		},
		"gcs with no prefix": {
			"content:\n  type: local\n  path: kb\n  memory:\n    type: gcs\n    bucket: b\n",
			"prefix is required",
		},
		"named collection names the offending path": {
			"collections:\n  - name: notes\n    type: local\n    path: kb\n    memory:\n      type: gcs\n",
			"collections[notes].memory.bucket is required",
		},
		// The data-loss guard: a relative local memory path under a
		// cache-backed source resolves inside a cache entry that a later
		// content change replaces.
		"relative path under a url source": {
			"content:\n  type: url\n  url: https://example.com/kb.tar.gz\n  sha256: " +
				strings.Repeat("a", 64) + "\n  memory:\n    type: local\n    path: m\n",
			"must be absolute",
		},
		"relative path under a gcs source": {
			"content:\n  type: gcs\n  bucket: b\n  prefix: kb/\n  memory:\n    type: local\n",
			"must be absolute",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.body), "content-source.yaml")
			if err == nil {
				t.Fatalf("parseConfig succeeded, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseConfig_MemoryOnALocalSourceMayBeRelative(t *testing.T) {
	// The common case: a durable content directory on disk, memories in
	// a sibling of wiki/.
	if _, err := parseConfig([]byte("content:\n  type: local\n  path: kb\n  memory:\n    type: local\n"), "c.yaml"); err != nil {
		t.Fatalf("a relative memory path under type: local is rejected: %v", err)
	}
}

func TestResolveRuntimeCollections_CarriesTheMemoryBlockThrough(t *testing.T) {
	// The resolved collection is what internal/collections builds the
	// registry from, so dropping memory: here would silently disable
	// writes for a deployment that asked for them.
	path := writeCfg(t, "content:\n  type: none\n  memory:\n    type: local\n    path: /srv/memory\n")
	cols, err := ResolveRuntimeCollections(context.Background(), path)
	if err != nil {
		t.Fatalf("ResolveRuntimeCollections: %v", err)
	}
	if len(cols) != 1 {
		t.Fatalf("got %d collections, want 1", len(cols))
	}
	if cols[0].Source.Memory == nil {
		t.Fatal("type: none dropped the memory: block")
	}
	if cols[0].Dir != "" || cols[0].Provenance != SourceEmbedded {
		t.Errorf("type: none no longer resolves to the embedded build: dir=%q provenance=%q", cols[0].Dir, cols[0].Provenance)
	}
}
