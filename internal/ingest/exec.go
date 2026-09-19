package ingest

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// ExecOpts configures a Run.
type ExecOpts struct {
	// WorkdirKB is the content working copy the executor writes to. Required.
	WorkdirKB string
	// Branch is the push target for write-back. Defaults to "main".
	Branch string
	// MaxParallel caps concurrent sessions. Defaults to 1.
	MaxParallel int
	// ExecutorName selects the default executor CLI: "opencode" (default)
	// or "claude". Ignored when Executor is set.
	ExecutorName string
	// AgentBin overrides the agent CLI binary path; empty uses the CLI's
	// default name resolved on PATH.
	AgentBin string
	// Out and Err receive per-page progress lines (one line per
	// task start + end). Defaults to os.Stdout / os.Stderr.
	Out io.Writer
	Err io.Writer
	// MaxConsecutiveFailures stops the run after this many failures in a
	// row. 0 means "never auto-stop".
	MaxConsecutiveFailures int
	// Executor runs each task. Defaults to the opencode executor.
	Executor Executor
	// TrustSources, when true, runs the agent CLI with
	// --dangerously-skip-permissions: the agent CLI runs without permission
	// prompts, so any instruction reachable from ingested content (a
	// malicious prompts/*.md in a source repo, for instance) executes
	// unchallenged, with push credentials in scope. Defaults to false.
	TrustSources bool
}

// Result is the outcome of one task.
type Result struct {
	Task       Task
	StartedAt  time.Time
	FinishedAt time.Time
	ExitStatus string // "ok" | "failed" | "timeout"
	FailReason string // populated when ExitStatus != "ok"
}

// ExecEnv is the resolved per-run environment shared by every task.
type ExecEnv struct {
	WorkdirKB string
	Branch    string
	Out, Err  io.Writer
	// TrustSources, when true, runs the agent CLI without permission
	// prompts (--dangerously-skip-permissions): any instruction reachable
	// from ingested content executes unchallenged. Defaults to false.
	TrustSources bool
}

// Executor runs a single ingestion Task and reports the outcome. The default
// implementation spawns `opencode run`; the interface lets a direct-SDK, CI,
// or mongoose-skill executor drop in without touching the planner or the Run
// control loop.
type Executor interface {
	// Name identifies the executor for diagnostics.
	Name() string
	// Exec processes one task within env and returns its Result.
	Exec(ctx context.Context, t Task, env ExecEnv) Result
}

// Run executes every task. Returns the per-task results in input
// order. Honours ctx cancellation between tasks (in-flight tasks
// finish or hit their wall-clock cap).
func Run(ctx context.Context, tasks []Task, opts ExecOpts) ([]Result, error) {
	if opts.WorkdirKB == "" {
		return nil, fmt.Errorf("WorkdirKB is required")
	}
	if opts.MaxParallel <= 0 {
		opts.MaxParallel = 1
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Err == nil {
		opts.Err = os.Stderr
	}
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	executor := opts.Executor
	if executor == nil {
		cli, err := newAgentCLI(opts.ExecutorName, opts.AgentBin)
		if err != nil {
			return nil, err
		}
		if _, err := exec.LookPath(cli.bin); err != nil {
			return nil, fmt.Errorf("%s binary not found: %w", cli.name, err)
		}
		executor = agentCLIExecutor{cli: cli}
	}
	env := ExecEnv{
		WorkdirKB:    opts.WorkdirKB,
		Branch:       opts.Branch,
		Out:          opts.Out,
		Err:          opts.Err,
		TrustSources: opts.TrustSources,
	}

	results := make([]Result, len(tasks))
	sem := make(chan struct{}, opts.MaxParallel)
	var wg sync.WaitGroup
	var consecutiveFails int64
	var stop atomic.Bool

	for i, t := range tasks {
		if stop.Load() || ctx.Err() != nil {
			break
		}
		i := i
		t := t
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r := executor.Exec(ctx, t, env)
			results[i] = r
			if r.ExitStatus == "ok" {
				atomic.StoreInt64(&consecutiveFails, 0)
				fmt.Fprintf(opts.Out, "  ok    [%4ds]  %s\n",
					int(r.FinishedAt.Sub(r.StartedAt).Seconds()), t.PageID)
			} else {
				n := atomic.AddInt64(&consecutiveFails, 1)
				fmt.Fprintf(opts.Err, "  %-5s [%4ds]  %s  — %s\n",
					r.ExitStatus, int(r.FinishedAt.Sub(r.StartedAt).Seconds()),
					t.PageID, r.FailReason)
				if opts.MaxConsecutiveFailures > 0 &&
					int(n) >= opts.MaxConsecutiveFailures {
					fmt.Fprintf(opts.Err,
						"!!! %d consecutive failures, stopping executor\n", n)
					stop.Store(true)
				}
			}
		}()
	}
	wg.Wait()
	return results, nil
}

// agentCLI describes how to invoke an external agent CLI for one page. The CLI
// fetches sources, writes the page, commits, and pushes; it holds its own
// model auth, so the meerkat binary stays credential-free.
type agentCLI struct {
	name string
	bin  string
	// buildCmd builds the command that runs instruction against env.WorkdirKB.
	buildCmd func(ctx context.Context, t Task, env ExecEnv, instruction string) *exec.Cmd
}

// newAgentCLI returns the agent CLI config for name ("" or "opencode" |
// "claude"), with an optional binary-path override.
func newAgentCLI(name, bin string) (agentCLI, error) {
	switch name {
	case "", "opencode":
		return opencodeCLI(bin), nil
	case "claude":
		return claudeCLI(bin), nil
	default:
		return agentCLI{}, fmt.Errorf("unknown executor %q (want opencode|claude)", name)
	}
}

// opencodeCLI spawns `opencode run ... --dir <workdir> <instruction>`.
func opencodeCLI(bin string) agentCLI {
	if bin == "" {
		bin = "opencode"
	}
	return agentCLI{name: "opencode", bin: bin,
		buildCmd: func(ctx context.Context, t Task, env ExecEnv, instruction string) *exec.Cmd {
			args := []string{"run"}
			if t.Model != "" {
				// Joined form, not "--model", t.Model: a model string from an
				// untrusted sources.yaml that begins with "-" must never be
				// parseable as a flag of its own (e.g. --dangerously-skip-permissions,
				// which TrustSources otherwise gates).
				args = append(args, "--model="+t.Model)
			}
			if env.TrustSources {
				args = append(args, "--dangerously-skip-permissions")
			}
			args = append(args, "--dir", env.WorkdirKB, "--title", "meerkat: "+t.PageID, instruction)
			return exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: fixed agent binary, no shell; args are page metadata.
		}}
}

// claudeCLI spawns Claude Code headlessly (`claude -p <instruction>`), with the
// working copy as the process cwd.
func claudeCLI(bin string) agentCLI {
	if bin == "" {
		bin = "claude"
	}
	return agentCLI{name: "claude", bin: bin,
		buildCmd: func(ctx context.Context, t Task, env ExecEnv, instruction string) *exec.Cmd {
			args := []string{"-p", instruction}
			if env.TrustSources {
				args = append(args, "--dangerously-skip-permissions")
			}
			if t.Model != "" {
				args = append(args, "--model="+t.Model) // joined form -- see opencodeCLI.
			}
			cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: fixed agent binary, no shell; args are page metadata.
			cmd.Dir = env.WorkdirKB                       // Claude Code uses cwd as the working dir.
			return cmd
		}}
}

// agentCLIExecutor runs one task by spawning the configured agent CLI.
type agentCLIExecutor struct{ cli agentCLI }

func (e agentCLIExecutor) Name() string { return e.cli.name }

func (e agentCLIExecutor) Exec(ctx context.Context, t Task, env ExecEnv) Result {
	r := Result{Task: t, StartedAt: time.Now()}

	// Skip pages that aren't placeholders any more (a parallel
	// worker may have processed them, or a manual edit happened).
	pagePath := filepath.Join(env.WorkdirKB, t.PagePath)
	if !isPathWithinBase(env.WorkdirKB, pagePath) {
		r.FinishedAt = time.Now()
		r.ExitStatus = "failed"
		r.FailReason = "task page_path escapes workdir"
		return r
	}
	if status, _ := readStatus(pagePath); t.Role == "" && status == "reviewed" {
		r.FinishedAt = time.Now()
		r.ExitStatus = "ok"
		r.FailReason = "skipped (already reviewed)"
		return r
	}

	wallClock := time.Duration(t.WallClockCapS) * time.Second
	if wallClock <= 0 {
		wallClock = time.Duration(DefaultWallClockCap) * time.Second
	}
	subCtx, cancel := context.WithTimeout(ctx, wallClock)
	defer cancel()

	instruction := buildInstruction(t, env.WorkdirKB, env.Branch)
	cmd := e.cli.buildCmd(subCtx, t, env, instruction)

	var combined strings.Builder
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	r.FinishedAt = time.Now()

	switch {
	case subCtx.Err() == context.DeadlineExceeded:
		r.ExitStatus = "timeout"
		r.FailReason = fmt.Sprintf("wall-clock cap %ds exceeded", t.WallClockCapS)
	case err != nil:
		r.ExitStatus = "failed"
		r.FailReason = trimErr(err.Error(), combined.String())
	default:
		// Verify the run did what the role requires.
		if reason := roleFailure(t, pagePath, e.cli.name); reason != "" {
			r.ExitStatus = "failed"
			r.FailReason = reason
		} else {
			r.ExitStatus = "ok"
		}
	}
	return r
}

// roleFailure is the post-run success check per role: "" means ok.
func roleFailure(t Task, pagePath, executor string) string {
	switch t.Role {
	case RoleResearcher:
		if _, err := os.Stat(pagePath); err != nil {
			return "no candidate page written by " + executor
		}
		if status, _ := readStatus(pagePath); status == "placeholder" || status == "ingest-failed" {
			return "candidate is " + status + " after " + executor + " run"
		}
		return ""
	case RoleValidator:
		b, err := os.ReadFile(pagePath) //nolint:gosec // G304: inside the working copy.
		if err != nil {
			return "candidate page missing after " + executor + " run"
		}
		p, err := kb.ParsePage(t.PageID, t.PagePath, b)
		if err != nil {
			return "candidate does not parse after " + executor + " run: " + err.Error()
		}
		if len(p.Front.Verified) == 0 && p.Front.FailureReason == "" {
			return executor + " neither verified nor failed the candidate"
		}
		return ""
	}
	if status, _ := readStatus(pagePath); status == "placeholder" {
		return "still placeholder after " + executor + " run"
	}
	return ""
}

// buildInstruction dispatches on the task's role.
func buildInstruction(t Task, workdir, branch string) string {
	switch t.Role {
	case RoleResearcher, RoleValidator:
		return buildRoleInstruction(t, workdir, branch)
	}
	return buildPerPageInstruction(t, workdir, branch)
}

func buildRoleInstruction(t Task, workdir, branch string) string {
	commitMsg := fmt.Sprintf("intake(%s): %s %s", t.Role, t.IntakeID, idBasename(t.PageID))
	return fmt.Sprintf(`You are the Meerkat %s agent for **one intake item**.
Working directory: %s
Candidate page: `+"`%s`"+` (id: `+"`%s`"+`, intake id `+"`%s`"+`)
The role prompt follows. Do exactly this one item, commit + push, then stop.
**IMPORTANT: Do this work yourself. DO NOT use the Task tool to delegate.**
After writing, run (in this order, with retry-on-conflict up to 3 times):
    git pull --rebase --quiet || true
    git add %s
    git commit -m "%s"
    git push origin %s
When done, print exactly: ITEM_DONE: %s
Then stop. No further items.
---
%s`, t.Role, workdir, t.PagePath, t.PageID, t.IntakeID, t.PagePath, commitMsg, branch, t.IntakeID, t.Prompt)
}

func isPathWithinBase(base, path string) bool {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(baseAbs, pathAbs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// buildPerPageInstruction wraps the rendered Source.Prompt with a
// minimal "do exactly this one page" header. This keeps each
// opencode session bounded and lets us run many in parallel.
func buildPerPageInstruction(t Task, workdir, branch string) string {
	commitMsg := fmt.Sprintf("ingest(%s): %s", t.SourceID, idBasename(t.PageID))
	return fmt.Sprintf(`You are the Meerkat ingestion sub-agent for **one single page**.

Working directory: %s
Target page: `+"`%s`"+` (id: `+"`%s`"+`)

The detailed system prompt for this page type follows. Process exactly this one page, commit + push, then stop.

**IMPORTANT: Do this work yourself. DO NOT use the Task tool to delegate.**

After writing the page, run (in this order, with retry-on-conflict up to 3 times):
    git pull --rebase --quiet || true
    git add %s
    git commit -m "%s"
    git push origin %s

If you cannot complete the page, set frontmatter "status: ingest-failed" with a one-line "failure_reason:", commit + push that, and exit.

Append one JSONL line to ingestion/log.jsonl after success.

When done, print exactly: PAGE_DONE: %s

Then stop. No further pages.

---

%s`, workdir, t.PagePath, t.PageID, t.PagePath, commitMsg, branch, t.PageID, t.Prompt)
}

// readStatus reads the frontmatter status field without parsing
// the whole YAML. Returns "" if no status line is found.
func readStatus(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "status:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "status:")), nil
		}
		if strings.TrimSpace(line) == "---" && len(line) > 0 {
			// Past the closing fence.
			break
		}
	}
	return "", nil
}

// trimErr clamps a noisy combined-output blob to something useful
// in a status line.
func trimErr(errMsg, combined string) string {
	combined = strings.TrimSpace(combined)
	if combined == "" {
		return errMsg
	}
	const maxLen = 200
	if len(combined) > maxLen {
		combined = "…" + combined[len(combined)-maxLen:]
	}
	return errMsg + " | " + combined
}
