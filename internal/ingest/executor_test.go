package ingest

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeExecutor records how many tasks ran and returns a fixed status, so the
// Run control loop (ordering, parallelism cap, consecutive-failure stop) can
// be tested without spawning opencode.
type fakeExecutor struct {
	calls  int64
	status string // "ok" | "failed"
}

func (f *fakeExecutor) Name() string { return "fake" }

func (f *fakeExecutor) Exec(_ context.Context, t Task, _ ExecEnv) Result {
	atomic.AddInt64(&f.calls, 1)
	return Result{Task: t, ExitStatus: f.status}
}

func tasksN(n int) []Task {
	ts := make([]Task, n)
	for i := range ts {
		ts[i] = Task{PageID: string(rune('a'+i)) + "/p"}
	}
	return ts
}

func TestRun_UsesInjectedExecutor(t *testing.T) {
	fe := &fakeExecutor{status: "ok"}
	res, err := Run(context.Background(), tasksN(5), ExecOpts{
		WorkdirKB: "/tmp", Executor: fe, MaxParallel: 3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res) != 5 {
		t.Fatalf("got %d results, want 5", len(res))
	}
	if fe.calls != 5 {
		t.Errorf("executor called %d times, want 5", fe.calls)
	}
	for i, r := range res {
		if r.ExitStatus != "ok" || r.Task.PageID != tasksN(5)[i].PageID {
			t.Errorf("result %d out of order or wrong status: %+v", i, r)
		}
	}
}

func TestRun_StopsOnConsecutiveFailures(t *testing.T) {
	fe := &fakeExecutor{status: "failed"}
	_, err := Run(context.Background(), tasksN(10), ExecOpts{
		WorkdirKB: "/tmp", Executor: fe, MaxParallel: 1,
		MaxConsecutiveFailures: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// With parallel=1 and a 2-failure stop, the loop halts well before all 10.
	if fe.calls > 4 {
		t.Errorf("expected an early stop (~2-3 calls), got %d", fe.calls)
	}
}

func TestNewAgentCLI(t *testing.T) {
	if c, err := newAgentCLI("", ""); err != nil || c.name != "opencode" || c.bin != "opencode" {
		t.Errorf("default -> %+v err=%v", c, err)
	}
	if c, err := newAgentCLI("claude", ""); err != nil || c.name != "claude" || c.bin != "claude" {
		t.Errorf("claude -> %+v err=%v", c, err)
	}
	if c, err := newAgentCLI("opencode", "/custom/oc"); err != nil || c.bin != "/custom/oc" {
		t.Errorf("bin override -> %+v err=%v", c, err)
	}
	if _, err := newAgentCLI("jenkins", ""); err == nil {
		t.Error("expected error for an unknown executor")
	}
}

func TestAgentCLI_BuildCmd(t *testing.T) {
	task := Task{PageID: "adr/x", Model: "sonnet"}
	env := ExecEnv{WorkdirKB: "/wd"}

	oc := opencodeCLI("").buildCmd(context.Background(), task, env, "INSTR")
	if !hasArg(oc.Args, "--dir") || !hasArg(oc.Args, "/wd") || !hasArg(oc.Args, "INSTR") {
		t.Errorf("opencode args missing --dir/workdir/instruction: %v", oc.Args)
	}
	if oc.Dir != "" {
		t.Errorf("opencode should not set cwd, got %q", oc.Dir)
	}

	cl := claudeCLI("").buildCmd(context.Background(), task, env, "INSTR")
	if !hasArg(cl.Args, "-p") || !hasArg(cl.Args, "INSTR") {
		t.Errorf("claude args missing -p/instruction: %v", cl.Args)
	}
	if cl.Dir != "/wd" {
		t.Errorf("claude cwd = %q, want /wd", cl.Dir)
	}
	if !hasArg(cl.Args, "--model=sonnet") {
		t.Errorf("claude should pass --model=sonnet: %v", cl.Args)
	}
}

// TestAgentCLI_ModelCannotInjectAFlag proves a model string from an untrusted
// sources.yaml cannot smuggle in a flag of its own. Passed as a separate
// ("--model", value) pair, a value like --dangerously-skip-permissions could be
// re-parsed as a flag by the agent CLI, defeating the TrustSources gate above.
// The joined --model=<value> form binds it as a value regardless of parser.
func TestAgentCLI_ModelCannotInjectAFlag(t *testing.T) {
	task := Task{PageID: "adr/x", Model: "--dangerously-skip-permissions"}
	untrusted := ExecEnv{WorkdirKB: "/wd", TrustSources: false}

	for _, tc := range []struct {
		name string
		cli  agentCLI
	}{
		{"opencode", opencodeCLI("")},
		{"claude", claudeCLI("")},
	} {
		cmd := tc.cli.buildCmd(context.Background(), task, untrusted, "INSTR")
		if hasArg(cmd.Args, "--dangerously-skip-permissions") {
			t.Errorf("%s: hostile model injected a bare flag: %v", tc.name, cmd.Args)
		}
		if !hasArg(cmd.Args, "--model=--dangerously-skip-permissions") {
			t.Errorf("%s: model not passed in joined form: %v", tc.name, cmd.Args)
		}
	}
}

// TestAgentCLI_TrustSourcesGatesSkipPermissions proves --dangerously-skip-permissions
// is only ever passed when ExecEnv.TrustSources is true, for both executors. This is
// the gate that keeps ingested-content prompt injection from running with unchallenged
// permissions by default.
func TestAgentCLI_TrustSourcesGatesSkipPermissions(t *testing.T) {
	task := Task{PageID: "adr/x"}

	untrusted := ExecEnv{WorkdirKB: "/wd", TrustSources: false}
	trusted := ExecEnv{WorkdirKB: "/wd", TrustSources: true}

	if oc := opencodeCLI("").buildCmd(context.Background(), task, untrusted, "INSTR"); hasArg(oc.Args, "--dangerously-skip-permissions") {
		t.Errorf("opencode: --dangerously-skip-permissions present with TrustSources=false: %v", oc.Args)
	}
	if oc := opencodeCLI("").buildCmd(context.Background(), task, trusted, "INSTR"); !hasArg(oc.Args, "--dangerously-skip-permissions") {
		t.Errorf("opencode: --dangerously-skip-permissions missing with TrustSources=true: %v", oc.Args)
	}

	if cl := claudeCLI("").buildCmd(context.Background(), task, untrusted, "INSTR"); hasArg(cl.Args, "--dangerously-skip-permissions") {
		t.Errorf("claude: --dangerously-skip-permissions present with TrustSources=false: %v", cl.Args)
	}
	if cl := claudeCLI("").buildCmd(context.Background(), task, trusted, "INSTR"); !hasArg(cl.Args, "--dangerously-skip-permissions") {
		t.Errorf("claude: --dangerously-skip-permissions missing with TrustSources=true: %v", cl.Args)
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestBuildPerPageInstruction_PushesResolvedBranch(t *testing.T) {
	instr := buildPerPageInstruction(Task{PageID: "adr/x", SourceID: "adr", PagePath: "wiki/adr/x.md"}, "/wd", "trunk")
	if !strings.Contains(instr, "git push origin trunk") {
		t.Errorf("instruction should push to the resolved branch; got:\n%s", instr)
	}
	if strings.Contains(instr, "git push origin master") {
		t.Error("instruction must not hardcode master")
	}
}
