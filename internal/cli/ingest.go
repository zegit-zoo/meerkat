package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/ingest"
	"github.com/zegit-zoo/meerkat/internal/intake"
	"github.com/zegit-zoo/meerkat/internal/sources"
	"github.com/zegit-zoo/meerkat/internal/traversal"
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
		Short: "List the ingestion source registry",
		Long: `Print every source from the loaded knowledge base's
ingestion/sources.yaml.`,
		Args: cobra.NoArgs,
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
	role          string
	from          string
	namespace     string
	only          string
	apply         bool
	days          int
}

var iflags ingestFlags

func ingestFlagSet() *flagSet {
	// We use a local helper so flags can be re-read when tests run.
	// (Real implementation just uses a normal pflag set.)
	return ingestPFlags(&iflags)
}

func runIngest(cmd *cobra.Command, args []string) error {
	if iflags.role != "" {
		return runIngestRole(cmd)
	}
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

// runIngestRole is `mk ingest --role ...`: the intake pipeline's
// researcher, validator and librarian (meerkat-mob issue H).
func runIngestRole(cmd *cobra.Command) error {
	role, err := ingest.ParseRole(iflags.role)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var store *intake.Store
	if activeIntake != nil {
		ms, err := activeIntake.Open(ctx, "")
		if err != nil {
			return fmt.Errorf("intake: %w", err)
		}
		store = intake.New(ms)
	}

	if role == ingest.RoleLibrarian {
		return runLibrarian(cmd, ctx, store)
	}
	if store == nil {
		return errors.New("--role " + string(role) + " needs an intake: store in content-source.yaml")
	}
	if iflags.from != "intake" {
		return fmt.Errorf("--from %q is not supported; only intake", iflags.from)
	}
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
	cfg, _ := contentsource.Load(".")
	tasks, skips, err := ingest.PlanIntake(ctx, store, ingest.IntakePlanOpts{
		Role: role, Namespace: iflags.namespace, Only: iflags.only, Workdir: abs,
		WikiDir: cfg.Content.Layout.Wiki, Model: iflags.model, SubagentType: iflags.subagent, WallClockCap: iflags.wallClockCap,
	})
	if err != nil {
		return fmt.Errorf("plan %s: %w", role, err)
	}
	for _, s := range skips {
		fmt.Fprintf(cmd.ErrOrStderr(), "  skip  %s  — %s\n", s.IntakeID, s.Reason)
	}
	if len(tasks) == 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "no %s work in %s — nothing to do\n", role, store.Describe())
		return nil
	}
	if !iflags.execute {
		if err := ingest.WriteJSONL(cmd.OutOrStdout(), tasks); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "\nplanned %d %s tasks (add --execute to run)\n", len(tasks), role)
		return nil
	}
	if iflags.dryRun {
		for _, t := range tasks {
			fmt.Fprintf(cmd.OutOrStdout(), "would execute: %s %s -> %s  model=%s\n", t.Role, t.IntakeID, t.PagePath, t.Model)
		}
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "executing %d %s tasks  workdir=%s  branch=%s  parallel=%d\n", len(tasks), role, abs, branch, iflags.maxParallel)
	results, err := ingest.Run(ctx, tasks, ingest.ExecOpts{
		WorkdirKB: abs, Branch: branch, ExecutorName: iflags.executorName, MaxParallel: iflags.maxParallel,
		Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr(), MaxConsecutiveFailures: iflags.maxConsecFail, TrustSources: iflags.trustSources,
	})
	if err != nil {
		return err
	}
	fins, err := ingest.Finalize(ctx, store, abs, results, time.Now())
	if err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	var failed int
	for _, f := range fins {
		fmt.Fprintf(cmd.OutOrStdout(), "  %-9s %s  %s\n", f.Action, f.IntakeID, f.Detail)
		if f.Action == "failed" {
			failed++
		}
		if f.Action == "parked" {
			fmt.Fprintf(cmd.ErrOrStderr(), "needs-human: intake %s parked — %s\n", f.IntakeID, f.Detail)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d %s tasks failed", failed, len(fins), role)
	}
	return nil
}

func runLibrarian(cmd *cobra.Command, ctx context.Context, store *intake.Store) error {
	reg := registry()
	var log *traversal.Log
	if activeObservability != nil && activeObservability.TraversalLog != nil {
		l, err := traversal.Open(ctx, activeObservability.TraversalLog, traversal.NewS3Sink)
		if err != nil {
			return fmt.Errorf("observability.traversal_log: %w", err)
		}
		log = l
	}
	rep, err := ingest.Librarian(ctx, reg, store, ingest.LibrarianOpts{Log: log, Days: iflags.days})
	if err != nil {
		return err
	}
	rep.Write(cmd.OutOrStdout())
	fmt.Fprintf(cmd.ErrOrStderr(), "\n%d findings: %d dangling, %d stale, %d cull, %d missing links, %d need a human, %d promotions, %d prompt-quality; %d candidates fileable\n",
		len(rep.Findings), rep.Count(ingest.FindingDangling), rep.Count(ingest.FindingStale), rep.Count(ingest.FindingCull),
		rep.Count(ingest.FindingMissingLink), rep.Count(ingest.FindingNeedsHuman), rep.Count(ingest.FindingPromotion), rep.Count(ingest.FindingPromptQuality), len(rep.Fileable))
	if iflags.apply {
		applied, err := ingest.Apply(ctx, reg, store, rep)
		if err != nil {
			return err
		}
		for _, a := range applied {
			fmt.Fprintf(cmd.OutOrStdout(), "  %-12s %s  %s\n", a.Action, a.IntakeID, a.Detail)
		}
	} else if len(rep.Fileable) > 0 || len(rep.Promotions) > 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "nothing filed; add --apply to file the confirmed candidates and root pointers through their contracts")
	}
	if len(rep.PromptQuality) == 0 {
		return nil
	}
	if !iflags.execute {
		fmt.Fprintf(cmd.ErrOrStderr(), "%d prompt-quality findings; add --execute (with --workdir-kb) to run the rewrites through the executor\n", len(rep.PromptQuality))
		return nil
	}
	return runRewrites(cmd, ctx, rep)
}

// runRewrites executes the report's prompt-quality rewrites in the
// content working copy: one executor task per page and field, checked
// against a pre-run snapshot, plus a tool-description proposal file.
func runRewrites(cmd *cobra.Command, ctx context.Context, rep *ingest.Report) error {
	workdir := iflags.workdirKB
	branch := iflags.branch
	if workdir == "" {
		wc, err := contentsource.ResolveWorkingCopy(".", cmd.ErrOrStderr())
		if err != nil {
			return fmt.Errorf("resolve content working copy (or pass --workdir-kb): %w", err)
		}
		workdir = wc.Path
	}
	if branch == "" {
		branch = ingest.DefaultRewriteBranch
	}
	abs, _ := filepath.Abs(workdir)
	cfg, _ := contentsource.Load(".")
	now := time.Now()
	if rel, err := ingest.WriteToolProposal(rep, abs, now); err != nil {
		return err
	} else if rel != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "  %-12s %s  tool-description proposal for a merge request on meerkat\n", "proposal", rel)
	}
	tasks, skips, err := ingest.PlanRewrites(rep, ingest.RewritePlanOpts{
		Workdir: abs, WikiDir: cfg.Content.Layout.Wiki, Model: iflags.model, SubagentType: iflags.subagent, WallClockCap: iflags.wallClockCap, Now: func() time.Time { return now },
	})
	if err != nil {
		return fmt.Errorf("plan rewrites: %w", err)
	}
	for _, s := range skips {
		fmt.Fprintf(cmd.ErrOrStderr(), "  skip  %s  — %s\n", s.IntakeID, s.Reason)
	}
	if len(tasks) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no rewrite targets in this working copy — nothing to execute")
		return nil
	}
	if iflags.dryRun {
		for _, t := range tasks {
			fmt.Fprintf(cmd.OutOrStdout(), "would execute: rewrite %s in %s (%s)  model=%s\n", t.Field, t.PagePath, t.TargetKB, t.Model)
		}
		return nil
	}
	before, err := ingest.Snapshot(abs, tasks)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "executing %d rewrites  workdir=%s  branch=%s  parallel=%d\n", len(tasks), abs, branch, iflags.maxParallel)
	results, err := ingest.Run(ctx, tasks, ingest.ExecOpts{
		WorkdirKB: abs, Branch: branch, ExecutorName: iflags.executorName, MaxParallel: iflags.maxParallel,
		Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr(), MaxConsecutiveFailures: iflags.maxConsecFail, TrustSources: iflags.trustSources,
	})
	if err != nil {
		return err
	}
	done, err := ingest.FinalizeRewrites(ctx, abs, results, before)
	if err != nil {
		return fmt.Errorf("finalize rewrites: %w", err)
	}
	var bad int
	for _, d := range done {
		fmt.Fprintf(cmd.OutOrStdout(), "  %-12s %s  %s\n", d.Action, d.Page, d.Detail)
		if d.Action == "failed" || d.Action == "rejected" {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d rewrites failed or were rejected", bad, len(done))
	}
	return nil
}
