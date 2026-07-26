package cli

import (
	"github.com/spf13/pflag"
)

// flagSet is just an alias for pflag.FlagSet so ingest.go can refer
// to it without importing pflag everywhere.
type flagSet = pflag.FlagSet

// ingestPFlags binds the ingest CLI flags to the shared iflags
// struct. Kept separate so the ingest command stays readable.
func ingestPFlags(f *ingestFlags) *flagSet {
	fs := pflag.NewFlagSet("ingest", pflag.ContinueOnError)
	fs.StringVar(&f.source, "source", "",
		"Restrict to one source id from sources.yaml (e.g. policies, adr, runbooks).")
	fs.StringVar(&f.page, "page", "",
		"Plan exactly one page (e.g. 'concepts/Rate-Limiting' or 'wiki/policies/foo.md'). Overrides --source/--statuses.")
	fs.StringSliceVar(&f.statuses, "status", nil,
		"Restrict to pages with these frontmatter statuses (default: placeholder,ingest-failed).")
	fs.StringVar(&f.model, "model", "",
		"Model override (default: openai/gpt-5.5-fast or per-source override in sources.yaml).")
	fs.StringVar(&f.subagent, "subagent", "",
		"OpenCode subagent type (default: general).")
	fs.BoolVar(&f.execute, "execute", false,
		"Actually run the tasks (otherwise plan-only).")
	fs.BoolVar(&f.dryRun, "dry-run", false,
		"With --execute, print the planned commands without running them.")
	fs.StringVar(&f.batchFile, "batch-file", "",
		"Plan-only: write the JSONL batch to this file instead of stdout.")
	fs.IntVar(&f.maxParallel, "max-parallel", 1,
		"Max concurrent opencode sessions when --execute (default 1).")
	fs.IntVar(&f.wallClockCap, "wall-clock-cap", 300,
		"Per-page wall-clock cap in seconds.")
	fs.StringVar(&f.workdirKB, "workdir-kb", "",
		"Content working copy to write to. Overrides the source resolved from content-source.yaml.")
	fs.StringVar(&f.branch, "branch", "",
		"Push target branch. Overrides the branch derived from content-source.yaml.")
	fs.StringVar(&f.executorName, "executor", "opencode",
		"Agent CLI to run each page: opencode | claude.")
	fs.IntVar(&f.maxConsecFail, "max-consecutive-failures", 0,
		"Stop the executor after this many consecutive failures (0 = never auto-stop).")
	fs.BoolVar(&f.reverse, "reverse", false,
		"Process tasks in reverse ID order. Useful when running a "+
			"second batch alongside a forward one — collisions skip "+
			"cheaply because the executor checks page status before "+
			"spawning opencode.")
	fs.BoolVar(&f.trustSources, "trust-sources", false,
		"Run the agent CLI with permission prompts disabled; any instruction reachable from ingested content then executes unchallenged.")
	return fs
}
