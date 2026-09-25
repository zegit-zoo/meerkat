package cli

import (
	"bytes"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/kbdir"
)

// TestCompleteSourceIDs returns every source id from the embedded
// sources.yaml when the prefix is empty, and narrows on prefix.
func TestCompleteSourceIDs(t *testing.T) {
	all, _ := completeSourceIDs(nil, nil, "")
	if len(all) == 0 {
		t.Skip("no sources embedded")
	}
	if !slices.Contains(all, "policies") {
		t.Errorf("expected 'policies' in: %v", all)
	}
	if !slices.Contains(all, "rfc") {
		t.Errorf("expected 'rfc' in: %v", all)
	}

	// Prefix narrows to backend/frontend systems.
	systems, _ := completeSourceIDs(nil, nil, "back")
	for _, s := range systems {
		if !strings.HasPrefix(s, "back") {
			t.Errorf("non-prefix match: %q", s)
		}
	}
}

// TestCompletePageIDs without onlyStale should list every page;
// with onlyStale only placeholder/ingest-failed pages.
func TestCompletePageIDs(t *testing.T) {
	all, _ := completePageIDs(false)(nil, nil, "")
	if len(all) == 0 {
		t.Skip("no kb content embedded")
	}

	// Prefix narrows.
	concepts, _ := completePageIDs(false)(nil, nil, "concepts/")
	for _, p := range concepts {
		if !strings.HasPrefix(p, "concepts/") {
			t.Errorf("non-prefix match: %q", p)
		}
	}

	// onlyStale should be a strict subset of all pages.
	stale, _ := completePageIDs(true)(nil, nil, "")
	if len(stale) > len(all) {
		t.Errorf("stale (%d) > all (%d)", len(stale), len(all))
	}
}

// TestCompletePrefixes generates progressive slash segments.
func TestCompletePrefixes(t *testing.T) {
	all, _ := completePrefixes(nil, nil, "")
	// All entries end with /
	for _, p := range all {
		if !strings.HasSuffix(p, "/") {
			t.Errorf("prefix %q missing trailing /", p)
		}
	}
	if len(all) == 0 {
		t.Skip("no kb content embedded")
	}
	// systems/ should be present (any backend page would create it).
	if !slices.Contains(all, "systems/") {
		t.Errorf("expected 'systems/' in prefixes")
	}
}

// TestCompleteStatuses returns the documented enum, narrowable.
func TestCompleteStatuses(t *testing.T) {
	all, _ := completeStatuses(nil, nil, "")
	for _, want := range []string{"placeholder", "reviewed", "stale", "ingest-failed"} {
		if !slices.Contains(all, want) {
			t.Errorf("expected %q in statuses: %v", want, all)
		}
	}
	// 'p' narrows to placeholder.
	narrowed, _ := completeStatuses(nil, nil, "p")
	if !slices.Contains(narrowed, "placeholder") || slices.Contains(narrowed, "reviewed") {
		t.Errorf("prefix 'p' should narrow to placeholder, got: %v", narrowed)
	}
}

// TestCompleteCategories returns distinct frontmatter categories.
func TestCompleteCategories(t *testing.T) {
	all, _ := completeCategories(nil, nil, "")
	if len(all) == 0 {
		// No categories means no kb content embedded (content stripped).
		//
		t.Skip("no categories — no kb content embedded")
	}
	for _, want := range []string{"concepts", "systems", "policies"} {
		if !slices.Contains(all, want) {
			t.Errorf("expected %q in categories: %v", want, all)
		}
	}
}

// TestCompleteTypes exercises the OKF `type` completion (SPEC.md §4.1)
// against injected fixture pages, independent of whatever is embedded.
func TestCompleteTypes(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/tables/orders.md":    {Data: []byte("---\ntype: BigQuery Table\n---\n# Orders\n")},
		"content/tables/customers.md": {Data: []byte("---\ntype: BigQuery Table\n---\n# Customers\n")},
		"content/playbooks/oncall.md": {Data: []byte("---\ntype: Playbook\n---\n# Oncall\n")},
		"content/notype.md":           {Data: []byte("# No type\n")},
	})
	t.Cleanup(func() { kb.UseFS(nil) })

	all, _ := completeTypes(nil, nil, "")
	if !slices.Contains(all, "BigQuery Table") || !slices.Contains(all, "Playbook") {
		t.Errorf("expected BigQuery Table and Playbook in types: %v", all)
	}
	// Distinct: "BigQuery Table" appears on two pages but once in the list.
	count := 0
	for _, v := range all {
		if v == "BigQuery Table" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("BigQuery Table should be deduplicated, appeared %d times in %v", count, all)
	}

	narrowed, _ := completeTypes(nil, nil, "Big")
	if !slices.Contains(narrowed, "BigQuery Table") || slices.Contains(narrowed, "Playbook") {
		t.Errorf("prefix 'Big' should narrow to BigQuery Table only, got: %v", narrowed)
	}
}

// ---------------------------------------------------------------------
// #77: completion must resolve the KB from --kb-dir / --content-source,
// not just from MEERKAT_KB_DIR / MEERKAT_CONTENT_SOURCE.
//
// These drive cobra's hidden `__complete` command — the exact code path
// a shell runs at TAB time — rather than calling the completion
// functions directly, because the bug lived in that path: `__complete`
// sets DisableFlagParsing, so the root command's PersistentPreRunE ran
// with the flag variables still empty and resolved content from the
// environment only. Calling completePageIDs() directly cannot see that.
// ---------------------------------------------------------------------

// newCompletionKBDir writes the minimal content-repo-layout directory
// from issue #77: one page, whose ID is "concepts/Widgets".
func newCompletionKBDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "wiki", "concepts", "Widgets.md"),
		"---\ntitle: Widgets\ncategory: concepts\nstatus: reviewed\n---\n# Widgets\n\nWidgets are round gadgets.\n")
	return dir
}

// isolateCompletionEnv makes a completion test independent of the
// developer's own environment and of test execution order: no inherited
// MEERKAT_* content location, no discovery of a real
// <user-config-dir>/meerkat/content-source.yaml, no update nag, and the
// process-global KB put back to the embed afterwards.
func isolateCompletionEnv(t *testing.T) {
	t.Helper()
	resetKBToEmbedded(t)
	isolateContentCaches(t)
	t.Setenv("MEERKAT_NO_UPDATE_CHECK", "1")
	t.Setenv(kbdir.EnvVar, "")
	t.Setenv(contentsource.EnvVar, "")
}

// execComplete runs the real command tree's `__complete` with args and
// returns stdout and stderr separately. The split matters: stdout IS the
// completion protocol (one completion per line, then ":<directive>"), so
// a stray diagnostic there corrupts the shell's parse, while cobra's own
// "Completion ended with directive" trailer is on stderr by design.
func execComplete(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	root := NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"__complete"}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("__complete %v: %v\nstdout: %s\nstderr: %s", args, err, out.String(), errOut.String())
	}
	return out.String(), errOut.String()
}

// splitCompletions separates the completion lines from the trailing
// ":<directive>" line, and fails if stdout holds anything else — that
// would be output leaking onto the completion protocol.
func splitCompletions(t *testing.T, stdout string) (completions []string, directive string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, ":"):
			if directive != "" {
				t.Fatalf("more than one directive line on stdout:\n%s", stdout)
			}
			directive = line
		case directive != "":
			t.Fatalf("output after the directive line corrupts the completion protocol:\n%s", stdout)
		default:
			// Cobra's shell format is "value\tdescription"; page IDs
			// carry no description, but split defensively anyway.
			completions = append(completions, strings.SplitN(line, "\t", 2)[0])
		}
	}
	if directive == "" {
		t.Fatalf("no directive line on stdout:\n%s", stdout)
	}
	return completions, directive
}

// TestComplete_PageIDs_FromKBDirFlag covers every way --kb-dir can be
// typed: after the subcommand (space- and =-separated) and before
// __complete, where cobra parses it on the root. All three failed
// identically before the fix, and for the same reason.
func TestComplete_PageIDs_FromKBDirFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(dir string) []string
	}{
		{"space separated", func(dir string) []string { return []string{"show", "--kb-dir", dir, "con"} }},
		{"equals separated", func(dir string) []string { return []string{"show", "--kb-dir=" + dir, "con"} }},
		{"before the command", func(dir string) []string { return []string{"--kb-dir", dir, "show", "con"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCompletionEnv(t)
			dir := newCompletionKBDir(t)

			stdout, _ := execComplete(t, tc.args(dir)...)
			got, directive := splitCompletions(t, stdout)
			if !slices.Contains(got, "concepts/Widgets") {
				t.Errorf("expected 'concepts/Widgets' from --kb-dir %s, got %v", dir, got)
			}
			if want := ":" + strconv.Itoa(int(cobra.ShellCompDirectiveNoFileComp)); directive != want {
				t.Errorf("directive = %q, want %q", directive, want)
			}
		})
	}
}

// TestComplete_PageIDs_FromContentSourceFlag is the same for a
// --content-source pointing at a type: local content-source.yaml.
func TestComplete_PageIDs_FromContentSourceFlag(t *testing.T) {
	isolateCompletionEnv(t)
	dir := newCompletionKBDir(t)
	cfg := filepath.Join(t.TempDir(), contentsource.ConfigFile)
	write(t, cfg, "content:\n  type: local\n  path: "+dir+"\n")

	stdout, _ := execComplete(t, "show", "--content-source", cfg, "con")
	got, _ := splitCompletions(t, stdout)
	if !slices.Contains(got, "concepts/Widgets") {
		t.Errorf("expected 'concepts/Widgets' from --content-source %s, got %v", cfg, got)
	}
}

// TestComplete_PageIDs_FromEnv pins the behaviour that already worked:
// the environment variables must keep resolving completions, since the
// fix deliberately does no re-resolution when no flag is present.
func TestComplete_PageIDs_FromEnv(t *testing.T) {
	t.Run("MEERKAT_KB_DIR", func(t *testing.T) {
		isolateCompletionEnv(t)
		t.Setenv(kbdir.EnvVar, newCompletionKBDir(t))

		stdout, _ := execComplete(t, "show", "con")
		got, _ := splitCompletions(t, stdout)
		if !slices.Contains(got, "concepts/Widgets") {
			t.Errorf("expected 'concepts/Widgets' from %s, got %v", kbdir.EnvVar, got)
		}
	})

	t.Run("MEERKAT_CONTENT_SOURCE", func(t *testing.T) {
		isolateCompletionEnv(t)
		dir := newCompletionKBDir(t)
		cfg := filepath.Join(t.TempDir(), contentsource.ConfigFile)
		write(t, cfg, "content:\n  type: local\n  path: "+dir+"\n")
		t.Setenv(contentsource.EnvVar, cfg)

		stdout, _ := execComplete(t, "show", "con")
		got, _ := splitCompletions(t, stdout)
		if !slices.Contains(got, "concepts/Widgets") {
			t.Errorf("expected 'concepts/Widgets' from %s, got %v", contentsource.EnvVar, got)
		}
	})
}

// TestComplete_PageIDs_FlagBeatsEnv pins the precedence a normal run
// has: the flag wins over the environment variable, and --kb-dir wins
// over --content-source. Re-resolving at completion time must not
// invent a different order from the root command's.
func TestComplete_PageIDs_FlagBeatsEnv(t *testing.T) {
	isolateCompletionEnv(t)
	flagDir := newCompletionKBDir(t)
	envDir := t.TempDir()
	write(t, filepath.Join(envDir, "wiki", "concepts", "EnvOnly.md"), "---\ntitle: Env Only\n---\n# Env Only\n")
	t.Setenv(kbdir.EnvVar, envDir)

	stdout, _ := execComplete(t, "show", "--kb-dir", flagDir, "con")
	got, _ := splitCompletions(t, stdout)
	if !slices.Contains(got, "concepts/Widgets") {
		t.Errorf("--kb-dir should beat %s: got %v", kbdir.EnvVar, got)
	}
	if slices.Contains(got, "concepts/EnvOnly") {
		t.Errorf("%s content leaked past --kb-dir: %v", kbdir.EnvVar, got)
	}

	// --kb-dir also beats --content-source on the same command line.
	cfg := filepath.Join(t.TempDir(), contentsource.ConfigFile)
	write(t, cfg, "content:\n  type: local\n  path: "+envDir+"\n")
	stdout, _ = execComplete(t, "show", "--content-source", cfg, "--kb-dir", flagDir, "con")
	got, _ = splitCompletions(t, stdout)
	if !slices.Contains(got, "concepts/Widgets") || slices.Contains(got, "concepts/EnvOnly") {
		t.Errorf("--kb-dir should beat --content-source: got %v", got)
	}
}

// TestComplete_UnresolvableFlag_IsSilent is the other half of the
// contract: a flag pointing at nothing must produce no completions and
// no error text on stdout. A resolution error printed there would be
// read back by the shell as a candidate.
func TestComplete_UnresolvableFlag_IsSilent(t *testing.T) {
	isolateCompletionEnv(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	stdout, _ := execComplete(t, "show", "--kb-dir", missing, "con")
	got, directive := splitCompletions(t, stdout)
	if len(got) != 0 {
		t.Errorf("expected no completions for a missing --kb-dir, got %v", got)
	}
	if want := ":" + strconv.Itoa(int(cobra.ShellCompDirectiveNoFileComp)); directive != want {
		t.Errorf("directive = %q, want %q (never ShellCompDirectiveError, never file completion)", directive, want)
	}
}

// TestComplete_Flags_FromKBDirFlag covers the flag-value completions
// that read the same content — `mk list --prefix`, `--category`,
// `--type` — through the same __complete path.
func TestComplete_Flags_FromKBDirFlag(t *testing.T) {
	isolateCompletionEnv(t)
	dir := newCompletionKBDir(t)

	for _, tc := range []struct {
		flag string
		want string
	}{
		{"--prefix", "concepts/"},
		{"--category", "concepts"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			stdout, _ := execComplete(t, "list", "--kb-dir", dir, tc.flag, "")
			got, _ := splitCompletions(t, stdout)
			if !slices.Contains(got, tc.want) {
				t.Errorf("%s completion: expected %q, got %v", tc.flag, tc.want, got)
			}
		})
	}
}
