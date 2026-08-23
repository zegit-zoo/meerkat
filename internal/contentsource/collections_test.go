package contentsource

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// collections_test.go covers the multi-collection config surface: that
// every single-source file still parses and resolves exactly as it did
// (back-compat), and that a `collections:` file parses, validates and
// resolves into named collections.

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ConfigFile)
	write(t, path, body)
	return path
}

// TestParseConfig_SingleSourceStillResolvesToOneDefaultCollection is the
// back-compat anchor: a file with no mention of collections at all must
// come back as exactly one collection named "default".
func TestParseConfig_SingleSourceStillResolvesToOneDefaultCollection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFile)
	write(t, path, "content:\n  type: local\n  path: kb\n")

	cols, err := ResolveRuntimeCollections(context.Background(), path)
	if err != nil {
		t.Fatalf("ResolveRuntimeCollections: %v", err)
	}
	if len(cols) != 1 {
		t.Fatalf("got %d collections, want exactly 1", len(cols))
	}
	if cols[0].Name != DefaultCollectionName {
		t.Errorf("name = %q, want %q", cols[0].Name, DefaultCollectionName)
	}
	if want := filepath.Join(dir, "kb"); cols[0].Dir != want {
		t.Errorf("dir = %q, want %q (relative path resolves against the config file's directory)", cols[0].Dir, want)
	}
	if want := "disk:" + filepath.Join(dir, "kb"); cols[0].Provenance != want {
		t.Errorf("provenance = %q, want %q", cols[0].Provenance, want)
	}
}

// TestResolveRuntimeCollections_NoConfigIsOneEmbeddedCollection: the
// unconfigured default (no content-source.yaml anywhere) is still a
// single collection, serving the embed.
func TestResolveRuntimeCollections_NoConfigIsOneEmbeddedCollection(t *testing.T) {
	dir := t.TempDir()
	isolateConfigDir(t, dir)
	t.Chdir(t.TempDir())

	cols, err := ResolveRuntimeCollections(context.Background(), "")
	if err != nil {
		t.Fatalf("ResolveRuntimeCollections: %v", err)
	}
	if len(cols) != 1 || cols[0].Name != DefaultCollectionName {
		t.Fatalf("got %+v, want one collection named %q", cols, DefaultCollectionName)
	}
	if cols[0].Dir != "" {
		t.Errorf("dir = %q, want empty (embedded fallback)", cols[0].Dir)
	}
	if cols[0].Provenance != SourceEmbedded {
		t.Errorf("provenance = %q, want %q", cols[0].Provenance, SourceEmbedded)
	}
}

func TestParseConfig_Collections(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, ConfigFile)
	write(t, path, `collections:
  - name: runbooks
    type: local
    path: ./runbooks
  - name: architecture
    type: local
    path: ./architecture
    layout:
      wiki: docs
`)

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(cfg.Collections) != 2 {
		t.Fatalf("got %d collections, want 2", len(cfg.Collections))
	}
	if cfg.Collections[0].Name != "runbooks" || cfg.Collections[1].Name != "architecture" {
		t.Errorf("names = %q/%q, want runbooks/architecture in file order",
			cfg.Collections[0].Name, cfg.Collections[1].Name)
	}
	// Layout defaults apply per collection, and an explicit override wins.
	if got := cfg.Collections[0].Layout.Wiki; got != "wiki" {
		t.Errorf("collections[0].layout.wiki = %q, want the default %q", got, "wiki")
	}
	if got := cfg.Collections[1].Layout.Wiki; got != "docs" {
		t.Errorf("collections[1].layout.wiki = %q, want the override %q", got, "docs")
	}
	if got := cfg.Collections[1].Layout.Sources; got != "ingestion/sources.yaml" {
		t.Errorf("collections[1].layout.sources = %q, want the default (an override of one field must not clear the rest)", got)
	}
}

func TestParseConfig_CollectionsResolveInOrder(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, ConfigFile)
	write(t, path, "collections:\n  - name: a\n    type: local\n    path: a\n  - name: b\n    type: local\n    path: /abs/b\n")

	cols, err := ResolveRuntimeCollections(context.Background(), path)
	if err != nil {
		t.Fatalf("ResolveRuntimeCollections: %v", err)
	}
	if len(cols) != 2 || cols[0].Name != "a" || cols[1].Name != "b" {
		t.Fatalf("got %+v, want [a b] in configuration order", cols)
	}
	if want := filepath.Join(base, "a"); cols[0].Dir != want {
		t.Errorf("a.dir = %q, want %q", cols[0].Dir, want)
	}
	if cols[1].Dir != filepath.FromSlash("/abs/b") {
		t.Errorf("b.dir = %q, want the absolute path unchanged", cols[1].Dir)
	}
}

func TestParseConfig_CollectionsErrors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			"content and collections together",
			"content:\n  type: local\n  path: kb\ncollections:\n  - name: a\n    type: local\n    path: a\n",
			"mutually exclusive",
		},
		{
			"duplicate names",
			"collections:\n  - name: a\n    type: local\n    path: x\n  - name: a\n    type: local\n    path: y\n",
			"duplicate collection name",
		},
		{
			"missing name",
			"collections:\n  - type: local\n    path: x\n",
			"not a valid collection name",
		},
		{
			"name with a colon would collide with the qualified-ID form",
			"collections:\n  - name: a:b\n    type: local\n    path: x\n",
			"not a valid collection name",
		},
		{
			"name starting with a dash",
			"collections:\n  - name: -a\n    type: local\n    path: x\n",
			"not a valid collection name",
		},
		{
			"type none is not a collection",
			"collections:\n  - name: a\n    type: none\n",
			"type is required",
		},
		{
			"missing type",
			"collections:\n  - name: a\n    path: x\n",
			"type is required",
		},
		{
			"per-collection field error names the collection",
			"collections:\n  - name: runbooks\n    type: local\n",
			"collections[runbooks].path is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFile(writeCfg(t, tc.body))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidate_SingleSourceErrorsUnchanged guards the back-compat of the
// error messages themselves: the field-path prefixing added for
// collections must not have renamed any content.* key an existing
// deployment's runbook or error report refers to.
func TestValidate_SingleSourceErrorsUnchanged(t *testing.T) {
	cases := []struct {
		src     Source
		wantErr string
	}{
		{Source{Type: TypeLocal, Layout: defaultLayout()}, "content.path is required for type: local"},
		{Source{Type: TypeGit, Layout: defaultLayout()}, "content.repo is required for type: git"},
		{Source{Type: TypeSubmodule, Layout: defaultLayout()}, "content.submodule (path) is required for type: submodule"},
		{Source{Type: TypeURL, Layout: defaultLayout()}, "content.url is required for type: url"},
		{Source{Type: "bogus", Layout: defaultLayout()}, "content.type must be one of none|local|git|submodule|url|gcs"},
	}
	for _, tc := range cases {
		err := tc.src.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("Validate(%s) = %v, want it to contain %q", tc.src.Type, err, tc.wantErr)
		}
	}
}

// TestResolveRuntime_RejectsMultiCollectionConfig: the legacy
// single-source entry point must fail loudly rather than silently
// serving only the first of several collections.
func TestResolveRuntime_RejectsMultiCollectionConfig(t *testing.T) {
	path := writeCfg(t, "collections:\n  - name: a\n    type: local\n    path: a\n  - name: b\n    type: local\n    path: b\n")

	_, err := ResolveRuntime(path)
	if err == nil {
		t.Fatal("expected an error from the single-source entry point")
	}
	if !strings.Contains(err.Error(), "2 collections") {
		t.Errorf("error = %v, want it to name the collection count", err)
	}
}
