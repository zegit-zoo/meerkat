package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpecValidate(t *testing.T) {
	tests := []struct {
		name      string
		spec      *Spec
		ephemeral bool
		wantErr   string
	}{
		{"nil is fine", nil, false, ""},
		{"local with defaults", &Spec{Type: BackendLocal}, false, ""},
		{"local with a relative path", &Spec{Type: BackendLocal, Path: "memory"}, false, ""},
		{"local with an absolute path", &Spec{Type: BackendLocal, Path: "/srv/memory"}, false, ""},
		{"gcs", &Spec{Type: BackendGCS, Bucket: "b", Prefix: "kb/memory/"}, false, ""},

		{"no type", &Spec{}, false, "type is required"},
		{"unknown type", &Spec{Type: "s3"}, false, `must be local or gcs`},
		{"local with a bucket", &Spec{Type: BackendLocal, Bucket: "b"}, false, "bucket/prefix apply to type: gcs"},
		{"gcs with no bucket", &Spec{Type: BackendGCS, Prefix: "p/"}, false, "bucket is required"},
		{"gcs with a gs:// bucket", &Spec{Type: BackendGCS, Bucket: "gs://b/x", Prefix: "p/"}, false, "must be a bucket name"},
		{"gcs with no prefix", &Spec{Type: BackendGCS, Bucket: "b"}, false, "prefix is required"},
		{"gcs with a path", &Spec{Type: BackendGCS, Bucket: "b", Prefix: "p/", Path: "x"}, false, "path applies to type: local"},

		// The data-loss guard: a relative local path under a
		// cache-backed content source resolves inside a cache entry that
		// a later content change replaces.
		{"local relative under a cache-backed source", &Spec{Type: BackendLocal, Path: "memory"}, true, "must be absolute"},
		{"local default under a cache-backed source", &Spec{Type: BackendLocal}, true, "must be absolute"},
		{"local absolute under a cache-backed source", &Spec{Type: BackendLocal, Path: "/srv/memory"}, true, ""},
		{"gcs under a cache-backed source", &Spec{Type: BackendGCS, Bucket: "b", Prefix: "p/"}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate("collections[notes].memory", tc.ephemeral)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate succeeded, want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate = %v, want it to contain %q", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "collections[notes].memory") {
				t.Errorf("error %q does not name the config path", err)
			}
		})
	}
}

func TestSpecOpen_LocalPathResolution(t *testing.T) {
	ctx := context.Background()
	contentDir := t.TempDir()

	// A relative path resolves against the collection's content
	// directory, as a SIBLING of wiki/ — never inside it, so a staged
	// document can never be picked up by the content walker.
	store, err := (&Spec{Type: BackendLocal}).Open(ctx, contentDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.(*LocalStore).Close() }()
	want := "local:" + filepath.Join(contentDir, DefaultLocalPath)
	if store.Describe() != want {
		t.Errorf("Describe = %q, want %q", store.Describe(), want)
	}

	// A relative path with nothing to resolve against is an error, not a
	// silent write into the working directory.
	if _, err := (&Spec{Type: BackendLocal, Path: "memory"}).Open(ctx, ""); err == nil {
		t.Error("Open with a relative path and no content dir succeeded")
	}

	// A nil spec opens nothing at all: the collection stays read-only.
	got, err := (*Spec)(nil).Open(ctx, contentDir)
	if err != nil || got != nil {
		t.Errorf("nil spec Open = %v, %v; want nil, nil", got, err)
	}
}

func TestDefaultLocalPathIsNotInsideTheWikiTree(t *testing.T) {
	// A regression guard for the decision documented on
	// DefaultLocalPath: if the store ever moved under wiki/, a staged
	// artifact would be indexed on the next restart, turning "pending
	// review" into "published, one restart later".
	if strings.HasPrefix(DefaultLocalPath, "wiki") {
		t.Fatalf("DefaultLocalPath = %q, which is inside the served content tree", DefaultLocalPath)
	}
}
