package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/ingest"
	"github.com/zegit-zoo/meerkat/internal/sources"
)

func newIngestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Plan and execute ingestion of placeholder KB pages",
		Long: `Drive Meerkat's ingestion pipeline.

The default behaviour is plan-only: print the JSONL batch that
would be executed. Add --execute to spawn one opencode session
per page (the actual ingestion).

Examples:
  mk ingest --source policies               # plan only, all stale policy pages
  mk ingest --source policies --execute     # run them
  mk ingest --page concepts/Rate-Limiting --execute
  mk ingest --execute --max-parallel 4      # everything stale, 4-wide
  mk ingest --batch-file batch.jsonl        # plan to file, no execute
`,
	}
	cmd.AddCommand(newSourcesCmd())
	cmd.Flags().AddFlagSet(ingestFlagSet())
	cmd.RunE = runIngest

	// Smart completion for the most-used flags. cobra picks these up
	// only when the flag was registered via Flags() on the same cmd
	// (which AddFlagSet does for us).
	_ = cmd.RegisterFlagCompletionFunc("source", completeSourceIDs)
	_ = cmd.RegisterFlagCompletionFunc("page", completePageIDs(true))
	_ = cmd.RegisterFlagCompletionFunc("status", completeStatuses)

	return cmd
}

func newSourcesCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "List the embedded ingestion source registry",
		Long:  `Print every source from the embedded sources.yaml.`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			all, err := sources.All()
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(all)
			}
			for _, s := range all {
				ref := s.Repo
				if ref == "" {
					ref = s.Group
				}
				if ref == "" {
					ref = s.Path
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"%-20s  %-15s  %-30s  -> wiki/%s\n",
					s.ID, s.Type, ref, s.TargetCategory)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	return cmd
}

// --- ingest flags + runner ------------------------------------

type ingestFlags struct {
	source        string
	page          string
	statuses      []string
	model         string
	subagent      string
	execute       bool
	dryRun        bool
	batchFile     string
	maxParallel   int
	wallClockCap  int
	workdirKB     string
	branch        string
	executorName  string
	maxConsecFail int
	reverse       bool
	trustSources  bool
}

var iflags ingestFlags

func ingestFlagSet() *flagSet {
	// We use a local helper so flags can be re-read when tests run.
	// (Real implementation just uses a normal pflag set.)
	return ingestPFlags(&iflags)
}

func runIngest(cmd *cobra.Command, args []string) error {
	// The wiki dir comes from content-source.yaml (layout.wiki) so Task
	// page paths match the content repo's layout. Absent/none config falls
	// back to the default "wiki".
	cfg, _ := contentsource.Load(".")
	tasks, err := ingest.Plan(ingest.PlanOpts{
		SourceID:     iflags.source,
		PageID:       iflags.page,
		Statuses:     iflags.statuses,
		Model:        iflags.model,
		SubagentType: iflags.subagent,
		WallClockCap: iflags.wallClockCap,
		Reverse:      iflags.reverse,
		WikiDir:      cfg.Content.Layout.Wiki,
	})
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	if len(tasks) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no matching pages — nothing to do")
		return nil
	}

	// Plan-only: write JSONL.
	if !iflags.execute {
		var w = cmd.OutOrStdout()
		if iflags.batchFile != "" {
			f, err := os.Create(iflags.batchFile)
			if err != nil {
				return fmt.Errorf("open batch file: %w", err)
			}
			defer f.Close()
			w = f
		}
		if err := ingest.WriteJSONL(w, tasks); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "\nplanned %d tasks", len(tasks))
		if iflags.batchFile != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), " -> %s", iflags.batchFile)
		}
		fmt.Fprintln(cmd.ErrOrStderr())
		return nil
	}

	if iflags.dryRun {
		// Useful for "what would --execute do?" — print the
		// planned list without actually spawning anything.
		for _, t := range tasks {
			fmt.Fprintf(cmd.OutOrStdout(),
				"would execute: %-50s  source=%s  model=%s\n",
				t.PageID, t.SourceID, t.Model)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "\n%d tasks planned (dry-run)\n", len(tasks))
		return nil
	}

	// Execute mode: resolve the working copy + push branch. --workdir-kb /
	// --branch override; otherwise both derive from content-source.yaml.
	workdir := iflags.workdirKB
	branch := iflags.branch
	if workdir == "" {
		wc, err := contentsource.ResolveWorkingCopy(".", cmd.ErrOrStderr())
		if err != nil {
			return fmt.Errorf("resolve content working copy (or pass --workdir-kb): %w", err)
		}
		workdir = wc.Path
		if branch == "" {
			branch = wc.Branch
		}
	}
	if branch == "" {
		branch = "main"
	}
	abs, _ := filepath.Abs(workdir)
	fmt.Fprintf(cmd.ErrOrStderr(),
		"executing %d tasks  workdir=%s  branch=%s  parallel=%d  cap=%ds/page\n",
		len(tasks), abs, branch, iflags.maxParallel, iflags.wallClockCap)

	if iflags.trustSources {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"warning: --trust-sources is set: agent permission prompts are disabled.\n"+
				"Content from ingested sources will be executed without confirmation.")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	results, err := ingest.Run(ctx, tasks, ingest.ExecOpts{
		WorkdirKB:              abs,
		Branch:                 branch,
		ExecutorName:           iflags.executorName,
		MaxParallel:            iflags.maxParallel,
		Out:                    cmd.OutOrStdout(),
		Err:                    cmd.ErrOrStderr(),
		MaxConsecutiveFailures: iflags.maxConsecFail,
		TrustSources:           iflags.trustSources,
	})
	if err != nil {
		return err
	}

	// Summary.
	var ok, failed, timeout int
	var totalDur time.Duration
	for _, r := range results {
		totalDur += r.FinishedAt.Sub(r.StartedAt)
		switch r.ExitStatus {
		case "ok":
			ok++
		case "failed":
			failed++
		case "timeout":
			timeout++
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"\n=== %d ok, %d failed, %d timeout (cumulative %s)\n",
		ok, failed, timeout, totalDur.Round(time.Second))
	if failed+timeout > 0 {
		os.Exit(1)
	}
	return nil
}
