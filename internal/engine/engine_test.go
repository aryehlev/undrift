package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/pullrequest"
	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/tasks"
)

// --- test harness ---------------------------------------------------------

type fixture struct {
	t          *testing.T
	dir        string
	upstream   string
	downstream string
	remote     string
	baseline   string
	e          *Engine
	prs        *fakeProvider
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo, msg string, files map[string]string) string {
	t.Helper()
	for p, c := range files {
		if c == "" {
			os.Remove(filepath.Join(repo, p))
			continue
		}
		write(t, filepath.Join(repo, p), c)
	}
	run(t, repo, "git", "add", "-A")
	run(t, repo, "git", "commit", "-q", "-m", msg)
	return run(t, repo, "git", "rev-parse", "HEAD")
}

// agentScript is a custom "coding agent": it ports a change by recording the
// task ID in ported.txt and adds a regression test that fails without it.
// MODE=fail makes it break the build; MODE=noop reports already_implemented.
const agentScript = `#!/bin/sh
set -e
task=$(basename "$(dirname "$UNDRIFT_RESULT_FILE")")
mode=$(cat "$AGENT_MODE_FILE" 2>/dev/null || echo ok)
grep -q "Undrift synchronization task" "$UNDRIFT_PROMPT_FILE"
case "$mode" in
fail)
  echo "broken" > BROKEN
  cat > "$UNDRIFT_RESULT_FILE" <<EOF
{"status":"implemented","summary":"broke it","confidence":0.9}
EOF
  ;;
noop)
  cat > "$UNDRIFT_RESULT_FILE" <<EOF
{"status":"already_implemented","summary":"ported.txt already handles this","confidence":0.95}
EOF
  ;;
*)
  echo "$task" >> ported.txt
  mkdir -p tests
  printf 'grep -q "%s" ported.txt\n' "$task" > "tests/$task.sh"
  cat > "$UNDRIFT_RESULT_FILE" <<EOF
{"status":"implemented","summary":"ported $task","tests_added":["tests/$task.sh"],"confidence":0.9,
 "mappings":[{"upstream":["auction/auction.go"],"downstream":["ported.txt"],"kind":"file","relationship":"auction logic"}],
 "notes":["ported.txt records synchronized tasks"]}
EOF
  ;;
esac
`

type fakeProvider struct {
	mu     sync.Mutex
	next   int
	prs    map[int]*state.PRRef
	heads  map[string]int
	bases  map[int]string
	bodies map[int]string
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{next: 1, prs: map[int]*state.PRRef{}, heads: map[string]int{}, bases: map[int]string{}, bodies: map[int]string{}}
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) FindByHead(_ context.Context, head string) (*state.PRRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.heads[head]; ok {
		cp := *f.prs[n]
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeProvider) Create(_ context.Context, r pullrequest.Request) (*state.PRRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.next
	f.next++
	f.prs[n] = &state.PRRef{Number: n, URL: fmt.Sprintf("https://example.test/pr/%d", n), State: "open", Draft: r.Draft}
	f.heads[r.Head] = n
	f.bases[n] = r.Base
	f.bodies[n] = r.Body
	cp := *f.prs[n]
	return &cp, nil
}
func (f *fakeProvider) Update(_ context.Context, n int, r pullrequest.Request) (*state.PRRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bases[n] = r.Base
	f.bodies[n] = r.Body
	cp := *f.prs[n]
	return &cp, nil
}
func (f *fakeProvider) Get(_ context.Context, n int) (*state.PRRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *f.prs[n]
	return &cp, nil
}
func (f *fakeProvider) Merge(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[n].State = "merged"
	return nil
}
func (f *fakeProvider) merge(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[n].State = "merged"
}

func setup(t *testing.T, extraConfig string) *fixture {
	t.Helper()
	dir := t.TempDir()
	gc := filepath.Join(dir, "gitconfig")
	write(t, gc, "[user]\n\tname = Test\n\temail = test@example.com\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n[protocol \"file\"]\n\tallow = always\n")
	t.Setenv("GIT_CONFIG_GLOBAL", gc)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	f := &fixture{t: t, dir: dir, upstream: filepath.Join(dir, "upstream"), downstream: filepath.Join(dir, "downstream"), remote: filepath.Join(dir, "remote.git")}
	os.MkdirAll(f.upstream, 0o755)
	run(t, f.upstream, "git", "init", "-q", "-b", "main")
	f.baseline = commit(t, f.upstream, "initial", map[string]string{
		"auction/auction.go": "package auction\n\nfunc RunAuction() {}\n",
		"README.md":          "# upstream\n",
	})

	os.MkdirAll(f.downstream, 0o755)
	run(t, f.downstream, "git", "init", "-q", "-b", "main")
	commit(t, f.downstream, "port", map[string]string{"src/engine.sh": "echo engine\n", "ported.txt": "baseline\n"})
	run(t, dir, "git", "init", "-q", "--bare", f.remote)
	run(t, f.downstream, "git", "remote", "add", "origin", f.remote)
	run(t, f.downstream, "git", "push", "-q", "origin", "main")

	write(t, filepath.Join(dir, "agent.sh"), agentScript)
	cfg := fmt.Sprintf(`version: 1
upstream:
  repository: %s
  branch: main
  baseline: %s
agent:
  provider: script
  timeout: 1m
agents:
  script:
    command: sh
    args: ["%s"]
    env: ["AGENT_MODE_FILE"]
verification:
  commands:
    - name: build
      run: test ! -f BROKEN
      level: build
    - name: tests
      run: for f in tests/*.sh; do [ -f "$f" ] || continue; sh "$f" || exit 1; done
      level: test
pull_requests:
  provider: github
limits:
  max_attempts_per_change: 2
%s`, f.upstream, f.baseline, filepath.Join(dir, "agent.sh"), extraConfig)
	write(t, filepath.Join(f.downstream, "undrift.yaml"), cfg)
	write(t, filepath.Join(f.downstream, ".undrift", ".gitignore"), "run/\nstate/\n")
	commit(t, f.downstream, "add undrift", nil)
	run(t, f.downstream, "git", "push", "-q", "origin", "main")
	t.Setenv("AGENT_MODE_FILE", filepath.Join(dir, "agent-mode"))
	f.open()
	return f
}

func (f *fixture) open() {
	cfg, err := config.Load(filepath.Join(f.downstream, "undrift.yaml"))
	if err != nil {
		f.t.Fatal(err)
	}
	var out io.Writer = io.Discard
	if testing.Verbose() {
		out = os.Stderr
	}
	e, err := Open(cfg, out)
	if err != nil {
		f.t.Fatal(err)
	}
	if f.prs == nil {
		f.prs = newFakeProvider()
	}
	e.SetProvider(f.prs)
	f.e = e
}

func (f *fixture) mode(m string) { write(f.t, filepath.Join(f.dir, "agent-mode"), m) }

func (f *fixture) change(sha string) *state.Change {
	f.t.Helper()
	c, err := f.e.Store.FindChange(sha[:12])
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) prepare(ctx context.Context) {
	f.t.Helper()
	if _, err := f.e.Scan(ctx, ScanOptions{}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.e.Analyze(ctx, AnalyzeOptions{}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.e.Plan(ctx); err != nil {
		f.t.Fatal(err)
	}
}

// --- tests ----------------------------------------------------------------

func TestPipelineEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	fix1 := commit(t, f.upstream, "fix: handle bidder timeout in auction", map[string]string{"auction/auction.go": "package auction\n\nfunc RunAuction() { /* timeout */ }\n"})
	docs := commit(t, f.upstream, "docs: clarify README", map[string]string{"README.md": "# upstream\n\nmore\n"})
	fix2 := commit(t, f.upstream, "fix: guard against nil bidder response", map[string]string{"auction/auction.go": "package auction\n\nfunc RunAuction() { /* timeout, nil */ }\n"})
	feat := commit(t, f.upstream, "feat: add bidder statistics", map[string]string{"stats/stats.go": "package stats\n"})

	res, err := f.e.Scan(ctx, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 4 {
		t.Fatalf("discovered %d, want 4", res.Total)
	}
	// Idempotent scan.
	if res, err = f.e.Scan(ctx, ScanOptions{}); err != nil || res.Total != 0 {
		t.Fatalf("rescan discovered %d (%v)", res.Total, err)
	}

	if _, err := f.e.Analyze(ctx, AnalyzeOptions{}); err != nil {
		t.Fatal(err)
	}
	if c := f.change(docs); c.Status != state.StatusSkipped || c.Analysis.Classification != state.ClassDocumentation {
		t.Fatalf("docs change: %s %s", c.Status, c.Analysis.Classification)
	}
	if c := f.change(fix1); c.Analysis.Classification != state.ClassBugFix || c.Status != state.StatusAnalyzed {
		t.Fatalf("fix1: %s %s", c.Status, c.Analysis.Classification)
	}
	if c := f.change(feat); c.Analysis.Classification != state.ClassFeature {
		t.Fatalf("feat: %s", c.Analysis.Classification)
	}

	plan, err := f.e.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Created) != 3 || len(plan.Ready) != 2 || len(plan.Waiting) != 1 {
		t.Fatalf("plan: created %d ready %d waiting %d", len(plan.Created), len(plan.Ready), len(plan.Waiting))
	}
	if c := f.change(fix2); len(c.DependsOn) != 1 || c.DependsOn[0] != f.change(fix1).ID || c.Status != state.StatusWaitingDependency {
		t.Fatalf("fix2 deps %v status %s", c.DependsOn, c.Status)
	}
	// Planning is idempotent.
	if p2, _ := f.e.Plan(ctx); len(p2.Created) != 0 {
		t.Fatalf("replan created %d tasks", len(p2.Created))
	}

	sr, err := f.e.Sync(ctx, SyncOptions{Parallel: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.Processed) != 3 {
		t.Fatalf("processed %d tasks: %+v", len(sr.Processed), sr.Processed)
	}
	for _, p := range sr.Processed {
		if p.Outcome != OutcomePROpened {
			t.Fatalf("task %s outcome %s: %s", p.Task, p.Outcome, p.Message)
		}
	}
	c1, c2 := f.change(fix1), f.change(fix2)
	if c1.Status != state.StatusPROpened || c1.PR == nil {
		t.Fatalf("fix1 status %s", c1.Status)
	}
	t1, _ := f.e.Store.GetTask(c1.TaskID)
	t2, _ := f.e.Store.GetTask(c2.TaskID)
	if t2.StackedOn != t1.ID || f.prs.bases[t2.PR.Number] != t1.Branch {
		t.Fatalf("fix2 should stack on fix1: stacked=%q base=%q", t2.StackedOn, f.prs.bases[t2.PR.Number])
	}
	if t1.Verification.Outcome != state.OutcomeVerified || t1.Verification.Regression == nil || !t1.Verification.Regression.Demonstrated {
		t.Fatalf("fix1 verification: %+v", t1.Verification)
	}
	// The stacked branch contains both ports.
	ported := run(t, f.downstream, "git", "show", t2.Commit+":ported.txt")
	if !strings.Contains(ported, t1.ID) || !strings.Contains(ported, t2.ID) {
		t.Fatalf("stacked branch content:\n%s", ported)
	}
	// Agent memory travels with the PR branch, not the main checkout.
	mem := run(t, f.downstream, "git", "show", t1.Commit+":.undrift/memory/mappings.yaml")
	if !strings.Contains(mem, "auction/auction.go") || !strings.Contains(mem, "source: agent") {
		t.Fatalf("mappings in branch:\n%s", mem)
	}
	body := f.prs.bodies[t1.PR.Number]
	for _, want := range []string{"Upstream change", "Verification", "undrift:task=" + t1.ID, "fail before"} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	msg := run(t, f.downstream, "git", "log", "-1", "--format=%B", t1.Commit)
	if !strings.Contains(msg, "Upstream-Change: "+c1.ID) {
		t.Fatalf("commit message: %s", msg)
	}
	// Opening a PR is not synchronization.
	st, _ := f.e.Status(ctx)
	if st.Changes[state.StatusMerged] != 0 {
		t.Fatal("nothing should be merged yet")
	}

	// Nothing left to do.
	if _, err := f.e.Sync(ctx, SyncOptions{}); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("second sync: %v", err)
	}

	// Merge PRs upstream of Undrift; reconcile finalizes changes.
	for n := range f.prs.prs {
		f.prs.merge(n)
	}
	tr, err := f.e.Reconcile(ctx)
	if err != nil || len(tr) != 3 {
		t.Fatalf("reconcile %d %v", len(tr), err)
	}
	st, _ = f.e.Status(ctx)
	if st.Changes[state.StatusMerged] != 3 || st.Remaining != 0 {
		t.Fatalf("status after merge: %+v", st.Changes)
	}
}

func TestExternalAgentFlowAndLeases(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	commit(t, f.upstream, "fix: crash on empty input", map[string]string{"auction/auction.go": "package auction\n// fixed\n"})
	f.prepare(ctx)

	ready, err := f.e.Next(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	id := ready[0].Task.ID
	cr, err := f.e.Claim(ctx, id, "worker-a", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Claim(ctx, id, "worker-b", time.Minute, false); !errors.Is(err, tasks.ErrLeaseHeld) {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cr.Workdir, "src", "engine.sh")); err != nil {
		t.Fatal("worktree not prepared")
	}
	prompt, _ := os.ReadFile(cr.PromptFile)
	for _, want := range []string{"<untrusted-upstream-content>", "fix: crash on empty input", "Acceptance criteria", cr.ResultFile} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// Agent "implements" without tests: verification passes, partially.
	write(t, filepath.Join(cr.Workdir, "ported.txt"), "baseline\nfixed\n")
	rep, err := f.e.Verify(ctx, id, cr.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != state.OutcomePartiallyVerified {
		t.Fatalf("outcome %s: %s", rep.Outcome, rep.Summary)
	}
	if _, err := f.e.Verify(ctx, id, "wrong-token", ""); !errors.Is(err, tasks.ErrLeaseHeld) {
		t.Fatalf("verify with wrong token: %v", err)
	}
	res, err := f.e.Complete(ctx, id, CompleteOptions{Token: cr.Token, Result: &state.AgentResult{Status: "implemented", Summary: "fixed it", Confidence: 0.8}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePROpened || res.PR == nil || !res.PR.Draft {
		t.Fatalf("complete: %+v", res)
	}
	tk, _ := f.e.Store.GetTask(id)
	if tk.Lease != nil {
		t.Fatal("lease should be released")
	}
}

func TestVerificationFailureRetriesThenFails(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	sha := commit(t, f.upstream, "fix: overflow in price calculation", map[string]string{"auction/auction.go": "package auction\n// overflow\n"})
	f.prepare(ctx)
	f.mode("fail")
	sr, err := f.e.Sync(ctx, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.Processed) != 2 || sr.Processed[0].Outcome != OutcomeRetry || sr.Processed[1].Outcome != OutcomeFailed {
		t.Fatalf("processed: %+v", sr.Processed)
	}
	c := f.change(sha)
	if c.Status != state.StatusFailed {
		t.Fatalf("status %s", c.Status)
	}
	tk, _ := f.e.Store.GetTask(c.TaskID)
	if len(tk.Attempts) != 2 || tk.Attempts[0].Outcome != "verification_failed" {
		t.Fatalf("attempts %+v", tk.Attempts)
	}
	// Previous failures are fed back to the next attempt.
	ctr, _ := f.e.BuildContract(ctx, tk)
	if len(ctr.PreviousAttempts) != 2 || len(ctr.PreviousAttempts[0].FailedChecks) == 0 {
		t.Fatalf("previous attempts not in contract: %+v", ctr.PreviousAttempts)
	}
	// Human retry with a fixed agent succeeds.
	f.mode("ok")
	if _, err := f.e.Retry(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	sr, err = f.e.Sync(ctx, SyncOptions{})
	if err != nil || len(sr.Processed) != 1 || sr.Processed[0].Outcome != OutcomePROpened {
		t.Fatalf("after retry: %+v %v", sr, err)
	}
}

func TestAlreadyImplementedAndHumanDecisions(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	a := commit(t, f.upstream, "fix: reject negative bids", map[string]string{"auction/auction.go": "package auction\n// neg\n"})
	b := commit(t, f.upstream, "refactor: rename internal helper", map[string]string{"auction/helpers.go": "package auction\n"})
	unclear := commit(t, f.upstream, "tweak things", map[string]string{"auction/misc.go": "package auction\n"})
	f.prepare(ctx)

	if c := f.change(b); c.Status != state.StatusSkipped {
		t.Fatalf("refactor should be skipped by policy, got %s", c.Status)
	}
	if c := f.change(unclear); c.Status != state.StatusNeedsReview {
		t.Fatalf("unclear change should need review, got %s (%s)", c.Status, c.StatusReason)
	}
	f.mode("noop")
	sr, err := f.e.Sync(ctx, SyncOptions{})
	if err != nil || len(sr.Processed) != 1 || sr.Processed[0].Outcome != OutcomeSettled {
		t.Fatalf("sync: %+v %v", sr, err)
	}
	if c := f.change(a); c.Status != state.StatusAlreadyImplemented || len(c.Decisions) == 0 {
		t.Fatalf("status %s", c.Status)
	}
	// Approve the unclear change; it becomes a task.
	if _, err := f.e.Approve(ctx, unclear[:8], "looks relevant"); err != nil {
		t.Fatal(err)
	}
	p, _ := f.e.Plan(ctx)
	if len(p.Created) != 1 {
		t.Fatalf("approved change not planned: %+v", p)
	}
	if _, err := f.e.Skip(ctx, unclear[:8], "decided against"); err != nil {
		t.Fatal(err)
	}
	tk, _ := f.e.Store.GetTask(f.change(unclear).TaskID)
	if tk.Status != state.TaskCancelled {
		t.Fatalf("task status after skip: %s", tk.Status)
	}
	h, _ := f.e.History(ctx, false, 0)
	if len(h) < 3 {
		t.Fatalf("history: %+v", h)
	}
}

func TestRevertAndRewrite(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	feat := commit(t, f.upstream, "feat: experimental bidding", map[string]string{"auction/exp.go": "package auction\n"})
	run(t, f.upstream, "git", "revert", "--no-edit", feat)
	res, err := f.e.Scan(ctx, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Upstreams[0].Reverted) != 2 {
		t.Fatalf("reverted: %+v", res.Upstreams[0])
	}
	if c := f.change(feat); c.Status != state.StatusReverted {
		t.Fatalf("status %s", c.Status)
	}

	// Rewrite: drop the two commits and add a new one.
	keep := commit(t, f.upstream, "fix: something real", map[string]string{"auction/auction.go": "package auction\n// real\n"})
	if _, err := f.e.Scan(ctx, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	run(t, f.upstream, "git", "reset", "-q", "--hard", f.baseline)
	run(t, f.upstream, "git", "cherry-pick", keep)
	newFix := run(t, f.upstream, "git", "rev-parse", "HEAD")
	_, err = f.e.Scan(ctx, ScanOptions{})
	if !errors.Is(err, ErrHistoryRewrite) {
		t.Fatalf("expected rewrite error, got %v", err)
	}
	res, err = f.e.Scan(ctx, ScanOptions{AcceptRewrite: true})
	if err != nil {
		t.Fatal(err)
	}
	rw := res.Upstreams[0].Rewrite
	if rw == nil || !rw.Handled || len(rw.Dropped) != 3 {
		t.Fatalf("rewrite: %+v", rw)
	}
	old := f.change(keep)
	if old.Status != state.StatusSuperseded || old.SupersededBy != f.change(newFix).ID {
		t.Fatalf("old %s superseded by %q", old.Status, old.SupersededBy)
	}
}

func TestStateBranchRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	commit(t, f.upstream, "fix: a", map[string]string{"auction/auction.go": "package auction\n// a\n"})
	f.prepare(ctx)
	bs := &state.BranchSync{Repo: f.e.Repo, Remote: "origin", Branch: "undrift/state", Dir: f.e.Store.Dir()}
	if err := bs.Push(ctx, "state"); err != nil {
		t.Fatal(err)
	}
	// A fresh clone (e.g. a CI runner) restores the ledger.
	clone := filepath.Join(f.dir, "clone")
	run(t, f.dir, "git", "clone", "-q", f.remote, clone)
	cfg, err := config.Load(filepath.Join(clone, "undrift.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	e2, err := Open(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	bs2 := &state.BranchSync{Repo: e2.Repo, Remote: "origin", Branch: "undrift/state", Dir: e2.Store.Dir()}
	n, err := bs2.Pull(ctx)
	if err != nil || n == 0 {
		t.Fatalf("pull %d %v", n, err)
	}
	st, _ := e2.Status(ctx)
	if st.Total != 1 {
		t.Fatalf("restored %d changes", st.Total)
	}
	// Re-scanning on the clone does not duplicate.
	if res, err := e2.Scan(ctx, ScanOptions{}); err != nil || res.Total != 0 {
		t.Fatalf("clone rescan %v %v", res, err)
	}
}

func TestGitLeasesAcrossMachines(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "state:\n  lease: git\n")
	commit(t, f.upstream, "fix: a", map[string]string{"auction/auction.go": "package auction\n// a\n"})
	f.prepare(ctx)
	bs := &state.BranchSync{Repo: f.e.Repo, Remote: "origin", Branch: "undrift/state", Dir: f.e.Store.Dir()}
	if err := bs.Push(ctx, "state"); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(f.dir, "machine-b")
	run(t, f.dir, "git", "clone", "-q", f.remote, clone)
	cfg, _ := config.Load(filepath.Join(clone, "undrift.yaml"))
	eb, _ := Open(cfg, io.Discard)
	if _, err := (&state.BranchSync{Repo: eb.Repo, Remote: "origin", Branch: "undrift/state", Dir: eb.Store.Dir()}).Pull(ctx); err != nil {
		t.Fatal(err)
	}
	ready, err := f.e.Next(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	id := ready[0].Task.ID
	if _, err := f.e.Tasks.Claim(ctx, id, "machine-a", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	// Machine B has a stale local view (task open) but the remote lease wins.
	if _, err := eb.Tasks.Claim(ctx, id, "machine-b", time.Minute, false); !errors.Is(err, tasks.ErrLeaseHeld) {
		t.Fatalf("machine b claim: %v", err)
	}
	if _, err := f.e.Tasks.Release(ctx, id, "", "machine-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := eb.Tasks.Claim(ctx, id, "machine-b", time.Minute, false); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}

func TestCrashRecoveryViaExpiredLease(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	commit(t, f.upstream, "fix: a", map[string]string{"auction/auction.go": "package auction\n// a\n"})
	f.prepare(ctx)
	ready, _ := f.e.Next(ctx, false)
	id := ready[0].Task.ID
	// A worker claims with a tiny lease and "crashes".
	if _, err := f.e.Claim(ctx, id, "crashed", time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	sr, err := f.e.Sync(ctx, SyncOptions{})
	if err != nil || len(sr.Processed) != 1 || sr.Processed[0].Outcome != OutcomePROpened {
		t.Fatalf("recovery sync: %+v %v", sr, err)
	}
}
