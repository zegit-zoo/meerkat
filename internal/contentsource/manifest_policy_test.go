package contentsource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/memory"
)

// isolateUserCache points os.UserCacheDir at a fresh directory and returns
// meerkat's cache directory under it.
func isolateUserCache(t *testing.T) string {
	t.Helper()
	c := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", c)
	t.Setenv("HOME", c)
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "meerkat", "content", "url", strings.Repeat("a", 64))
	if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestManifest_RefusesOperatorOnlyKeys: memory:, update: direct (on a
// child or as the manifest's contract), and refresh remote_check /
// on_divergence are refused in a manifest whatever the policy allows.
// Each case has a twin without the key that parses.
func TestManifest_RefusesOperatorOnlyKeys(t *testing.T) {
	const head = "kind: KnowledgeBase\nname: root\n"
	for _, tc := range []struct {
		name, ok, refused, want string
	}{
		{
			"child memory",
			head + "children:\n  - name: leaf\n    source: {type: local, path: /srv/kb/leaf}\n",
			head + "children:\n  - name: leaf\n    source: {type: local, path: /srv/kb/leaf, memory: {type: local, path: /srv/mem}}\n",
			"children[0].source.memory is refused",
		},
		{
			"child remote_check",
			head + "children:\n  - name: leaf\n    source: {type: local, path: /srv/kb/leaf, refresh: {interval: 1m}}\n",
			head + "children:\n  - name: leaf\n    source: {type: local, path: /srv/kb/leaf, refresh: {interval: 1m, remote_check: 1m}}\n",
			"remote_check and on_divergence are refused",
		},
		{
			"child on_divergence",
			head + "children:\n  - name: leaf\n    source: {type: local, path: /srv/kb/leaf, refresh: {interval: 1m}}\n",
			head + "children:\n  - name: leaf\n    source: {type: local, path: /srv/kb/leaf, refresh: {interval: 1m, remote_check: 1m, on_divergence: pull}}\n",
			"remote_check and on_divergence are refused",
		},
		{
			"child update direct",
			head + "children:\n  - name: leaf\n    source: {type: local, path: /srv/kb/leaf, update: {method: none}}\n",
			head + "children:\n  - name: leaf\n    source: {type: local, path: /srv/kb/leaf, update: {method: direct}}\n",
			"update: method: direct is refused",
		},
		{
			"contract direct",
			head + "contract: {method: none}\n",
			head + "contract: {method: direct}\n",
			"contract: method: direct is refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(tc.ok)); err != nil {
				t.Fatalf("twin without the key: %v", err)
			}
			_, err := ParseManifest([]byte(tc.refused))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestChildPolicy_Validate(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    ChildPolicy
		want string
	}{
		{"ok", ChildPolicy{Types: []string{"s3", "url", "local"}, Buckets: []string{"kb"}, Endpoints: []string{"https://s3.example.net"}, URLPrefixes: []string{"https://kb.example.net/b/"}, LocalPaths: []string{"/srv/kb"}}, ""},
		{"unknown type", ChildPolicy{Types: []string{"git"}}, "not a type"},
		{"bucket path", ChildPolicy{Buckets: []string{"kb/x"}}, "not a bucket name"},
		{"endpoint", ChildPolicy{Endpoints: []string{"s3.example.net"}}, "not an endpoint URL"},
		{"prefix http", ChildPolicy{URLPrefixes: []string{"http://kb.example.net/"}}, "https://"},
		{"prefix no slash", ChildPolicy{URLPrefixes: []string{"https://kb.example.net"}}, `must end in "/"`},
		{"prefix userinfo", ChildPolicy{URLPrefixes: []string{"https://u@kb.example.net/"}}, "user info"},
		{"relative local", ChildPolicy{LocalPaths: []string{"kb"}}, "absolute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate("manifest_children")
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseConfig_ManifestChildrenNeedsTree(t *testing.T) {
	if _, err := parseConfig([]byte("content: {type: local, path: kb}\nmanifest_children: {types: [local]}\n"), "t.yaml"); err == nil || !strings.Contains(err.Error(), "tree: deployment only") {
		t.Errorf("err = %v", err)
	}
	if _, err := parseConfig([]byte("tree: {type: local, path: kb}\nmanifest_children: {types: [git]}\n"), "t.yaml"); err == nil || !strings.Contains(err.Error(), "not a type") {
		t.Errorf("err = %v", err)
	}
}

// TestChildRules_Admit is the allowlist table: the default (no
// manifest_children:) admits only object-store children in the root's
// own bucket and endpoint; an explicit block admits what it lists.
func TestChildRules_Admit(t *testing.T) {
	isolateUserCache(t)
	allowed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(allowed, "leaf"), 0o755); err != nil {
		t.Fatal(err)
	}
	s3root := Source{Type: TypeS3, Bucket: "kb", Prefix: "kb/root/", Endpoint: "https://s3.example.net"}
	mem := memory.Spec{Type: memory.BackendS3, Bucket: "kb", Prefix: "kb/memory/", Endpoint: "https://s3.example.net"}
	def := newChildRules(nil, s3root, []memory.Spec{mem})
	pol := newChildRules(&ChildPolicy{
		Types:       []string{TypeS3, TypeURL, TypeLocal},
		Buckets:     []string{"archive"},
		Endpoints:   []string{"https://s3.other.net/"},
		URLPrefixes: []string{"https://kb.example.net/bundles/"},
		LocalPaths:  []string{allowed},
	}, s3root, nil)
	sha := strings.Repeat("b", 64)

	for _, tc := range []struct {
		name  string
		rules *childRules
		src   Source
		want  string // "" = admitted
	}{
		{"default: root bucket and endpoint", def, Source{Type: TypeS3, Bucket: "kb", Prefix: "kb/flux/", Endpoint: "https://s3.example.net"}, ""},
		{"default: provider endpoint", def, Source{Type: TypeS3, Bucket: "kb", Prefix: "kb/flux/"}, ""},
		{"default: other bucket", def, Source{Type: TypeS3, Bucket: "secrets", Prefix: "x/"}, `bucket "secrets" is not allowed`},
		{"default: other endpoint", def, Source{Type: TypeS3, Bucket: "kb", Prefix: "kb/flux/", Endpoint: "http://collector.example"}, "endpoint"},
		{"default: other type", def, Source{Type: TypeGCS, Bucket: "kb", Prefix: "kb/flux/"}, `type "gcs" is not allowed`},
		{"default: local", def, Source{Type: TypeLocal, Path: filepath.Join(allowed, "leaf")}, `type "local" is not allowed`},
		{"default: url", def, Source{Type: TypeURL, URL: "https://kb.example.net/bundles/a.tgz", SHA256: sha}, `type "url" is not allowed`},
		{"default: memory prefix", def, Source{Type: TypeS3, Bucket: "kb", Prefix: "kb/memory/personal/", Endpoint: "https://s3.example.net"}, "overlaps a memory or intake store"},
		{"default: bucket root over memory", def, Source{Type: TypeS3, Bucket: "kb", Prefix: "kb/", Endpoint: "https://s3.example.net"}, "overlaps a memory or intake store"},
		{"policy: listed bucket", pol, Source{Type: TypeS3, Bucket: "archive", Prefix: "a/"}, ""},
		{"policy: listed endpoint", pol, Source{Type: TypeS3, Bucket: "archive", Prefix: "a/", Endpoint: "https://S3.other.net"}, ""},
		{"policy: url under prefix", pol, Source{Type: TypeURL, URL: "https://kb.example.net/bundles/a.tgz", SHA256: sha}, ""},
		{"policy: url other path", pol, Source{Type: TypeURL, URL: "https://kb.example.net/other/a.tgz", SHA256: sha}, "not under an allowed prefix"},
		{"policy: url other host", pol, Source{Type: TypeURL, URL: "https://kb.example.net.evil.test/bundles/a.tgz", SHA256: sha}, "not under an allowed prefix"},
		{"policy: url dot segment", pol, Source{Type: TypeURL, URL: "https://kb.example.net/bundles/../x.tgz", SHA256: sha}, "not under an allowed prefix"},
		{"policy: url encoded dot", pol, Source{Type: TypeURL, URL: "https://kb.example.net/bundles/%2e%2e/x.tgz", SHA256: sha}, "not under an allowed prefix"},
		{"policy: url userinfo", pol, Source{Type: TypeURL, URL: "https://kb.example.net@evil.test/bundles/a.tgz", SHA256: sha}, "not under an allowed prefix"},
		{"policy: local under allowed", pol, Source{Type: TypeLocal, Path: filepath.Join(allowed, "leaf")}, ""},
		{"policy: local outside", pol, Source{Type: TypeLocal, Path: "/etc"}, "not under an allowed directory"},
		{"policy: local dot-dot out", pol, Source{Type: TypeLocal, Path: filepath.Join(allowed, "..", "x")}, "not under an allowed directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.rules.admit("child", tc.src, t.TempDir())
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestChildRules_LocalNeverInsideCache: a local child that resolves into
// meerkat's cache directory is refused — even with no allowlist at all,
// even when the operator allowlisted a directory that contains the
// cache, and even through a symlink planted in an allowed directory.
func TestChildRules_LocalNeverInsideCache(t *testing.T) {
	cached := isolateUserCache(t)
	allowed := t.TempDir()
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(cached, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	home := os.Getenv("HOME")
	for _, tc := range []struct {
		name  string
		rules *childRules
		path  string
	}{
		{"unrestricted", &childRules{unrestricted: true}, cached},
		{"allowlist contains the cache", newChildRules(&ChildPolicy{LocalPaths: []string{home}}, Source{}, nil), cached},
		{"symlink in an allowed dir", newChildRules(&ChildPolicy{LocalPaths: []string{allowed}}, Source{}, nil), link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.rules.admit("child", Source{Type: TypeLocal, Path: tc.path}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "inside meerkat's cache directory") {
				t.Fatalf("err = %v, want the cache refusal", err)
			}
		})
	}
}

// TestResolveRuntimeCollections_TreeLocalChildPolicy walks a real tree:
// a local child is refused without manifest_children:, and a relative
// child path resolves against the manifest's own directory, not the
// directory content-source.yaml sits in.
func TestResolveRuntimeCollections_TreeLocalChildPolicy(t *testing.T) {
	isolateUserCache(t)
	base := t.TempDir()
	kbs := filepath.Join(base, "kbs")
	kbDir(t, kbs, "leaf", "kind: KnowledgeBase\nname: leaf\n")
	root := kbDir(t, kbs, "root", "kind: KnowledgeBase\nname: root\nchildren:\n  - name: leaf\n    source: {type: local, path: ../leaf}\n")
	// A decoy beside the operator's config: if the relative path resolved
	// against the config directory, this is what would load.
	cfgDir := filepath.Join(base, "etc")
	kbDir(t, cfgDir, "leaf", "kind: KnowledgeBase\nname: leaf\n")
	cfgPath := filepath.Join(cfgDir, ConfigFile)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("tree:\n  type: local\n  path: " + root + "\n")
	if _, err := ResolveRuntimeCollections(context.Background(), cfgPath); err == nil || !strings.Contains(err.Error(), `type "local" is not allowed`) {
		t.Fatalf("no policy: err = %v, want the local child refused", err)
	}

	write("tree:\n  type: local\n  path: " + root + "\n" + allowLocal(kbs))
	cols, err := ResolveRuntimeCollections(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("with policy: %v", err)
	}
	if len(cols) != 2 || realPath(cols[1].Dir) != realPath(filepath.Join(kbs, "leaf")) {
		t.Fatalf("leaf dir = %q, want %q (the manifest's sibling, not the config's)", cols[1].Dir, filepath.Join(kbs, "leaf"))
	}

	// The decoy directory is outside the allowlist, so an operator who
	// allowlists only the config side does not get the manifest's child.
	write("tree:\n  type: local\n  path: " + root + "\n" + allowLocal(cfgDir))
	if _, err := ResolveRuntimeCollections(context.Background(), cfgPath); err == nil || !strings.Contains(err.Error(), "not under an allowed directory") {
		t.Fatalf("config-side allowlist: err = %v", err)
	}
}

// TestResolveSource_LazyManifestChildIsRechecked: a lazy local child is
// admitted at the walk, then its directory is swapped for a symlink into
// the cache before it is mounted; the mount is refused.
func TestResolveSource_LazyManifestChildIsRechecked(t *testing.T) {
	cached := isolateUserCache(t)
	base := t.TempDir()
	leaf := kbDir(t, base, "leaf", "kind: KnowledgeBase\nname: leaf\n")
	root := kbDir(t, base, "root", "kind: KnowledgeBase\nname: root\nchildren:\n"+child("leaf", leaf, "lazy"))
	cfgPath := filepath.Join(base, ConfigFile)
	if err := os.WriteFile(cfgPath, []byte("tree:\n  type: local\n  path: "+root+"\n"+allowLocal(base)), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_, tree, err := resolveTree(context.Background(), *cfg.Tree, cfgPath, cfg.childRules())
	if err != nil {
		t.Fatal(err)
	}
	src, from := tree.Nodes["leaf"].LazySource()
	if src == nil || from != filepath.Join(root, ManifestFile) {
		t.Fatalf("lazy source = %+v from %q", src, from)
	}
	if _, err := ResolveSource(context.Background(), *src, from); err != nil {
		t.Fatalf("mount before the swap: %v", err)
	}
	if err := os.RemoveAll(leaf); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cached, leaf); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if _, err := ResolveSource(context.Background(), *src, from); err == nil || !strings.Contains(err.Error(), "inside meerkat's cache directory") {
		t.Fatalf("mount after the swap: err = %v, want the cache refusal", err)
	}
}

func TestPrefixesNestAndWithin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"kb/memory/", "kb/memory/intake/", true},
		{"kb/memory", "kb/memory/", true},
		{"", "anything/", true},
		{"kb/mem/", "kb/memory/", false},
		{"kb/a/", "kb/b/", false},
	} {
		if got := prefixesNest(tc.a, tc.b); got != tc.want {
			t.Errorf("prefixesNest(%q,%q) = %v", tc.a, tc.b, got)
		}
	}
	if !within("/srv/kb/a", "/srv/kb") || within("/srv/kbx", "/srv/kb") || within("/srv", "/srv/kb") || !within("/srv/kb", "/srv/kb") {
		t.Error("within")
	}
}
