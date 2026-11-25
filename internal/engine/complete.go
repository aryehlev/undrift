package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aryehlev/undrift/internal/analyzer"
	"github.com/aryehlev/undrift/internal/git"
	"github.com/aryehlev/undrift/internal/memory"
	"github.com/aryehlev/undrift/internal/pullrequest"
	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/tasks"
	"github.com/aryehlev/undrift/internal/upstream"
	"github.com/aryehlev/undrift/internal/verifier"
	"github.com/aryehlev/undrift/internal/workspace"
)

var errNoChanges = errors.New("no downstream changes were made")

// Outcomes of completing a task.
const (
	OutcomePROpened    = "pr_opened"
	OutcomeMerged      = "merged"
	OutcomeVerified    = "verified"
	OutcomeRetry       = "retry"
	OutcomeFailed      = "failed"
	OutcomeNeedsReview = "needs_review"
	OutcomeSettled     = "settled"
)

// CompleteOptions carries an agent's report for a task.
type CompleteOptions struct {
	Token   string
	Owner   string
	Result  *state.AgentResult
	Agent   string
	Attempt *state.Attempt // pre-filled by managed execution (logs, cost, timing)
}

// CompleteResult reports what happened.
type CompleteResult struct {
	Task    *state.Task               `json:"task"`
	Outcome string                    `json:"outcome"`
	Message string                    `json:"message"`
	Report  *state.VerificationReport `json:"verification,omitempty"`
	PR      *state.PRRef              `json:"pr,omitempty"`
}

// protectPaths restores Undrift-managed files that an agent must not
// change (configuration and memory) to their base versions.
func protectPaths(ctx context.Context, dir, base string) {
	r := git.Open(dir)
	for _, p := range []string{"undrift.yaml", ".undrift"} {
		if _, err := r.Run(ctx, "cat-file", "-e", base+":"+p); err == nil {
			_, _ = r.Run(ctx, "checkout", base, "--", p)
			_, _ = r.Run(ctx, "clean", "-fdq", "--", p)
		} else {
			_ = os.RemoveAll(filepath.Join(dir, p))
		}
	}
}

// VerifyTask commits the workspace and runs verification. The report is
// cached by tree hash, so repeated verification of unchanged work is free.
// Agent edits to undrift.yaml and .undrift/ are discarded first unless
// protect is false (used after Undrift itself recorded memory updates).
func (e *Engine) VerifyTask(ctx context.Context, t *state.Task, result *state.AgentResult, protect bool) (*state.VerificationReport, error) {
	if t.Worktree == "" {
		return nil, fmt.Errorf("task %s has no workspace (claim it first)", t.ID)
	}
	if _, err := os.Stat(t.Worktree); err != nil {
		return nil, fmt.Errorf("workspace for task %s is missing: %w", t.ID, err)
	}
	if protect {
		protectPaths(ctx, t.Worktree, t.BaseSHA)
	}
	head, _, err := workspace.CommitAll(ctx, t.Worktree, "undrift: work in progress for "+t.ID)
	if err != nil {
		return nil, err
	}
	changed, err := workspace.ChangedFiles(ctx, t.Worktree, t.BaseSHA)
	if err != nil {
		return nil, err
	}
	if len(changed) == 0 {
		return nil, errNoChanges
	}
	r := git.Open(t.Worktree)
	tree, err := r.Run(ctx, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	if t.Verification != nil && t.Verification.Tree == tree {
		return t.Verification, nil
	}
	var testFiles []string
	if result != nil && len(result.TestsAdded) > 0 {
		for _, f := range result.TestsAdded {
			if contains(changed, f) {
				testFiles = append(testFiles, f)
			}
		}
	}
	if len(testFiles) == 0 {
		for _, f := range changed {
			if analyzer.IsTestFile(f) {
				testFiles = append(testFiles, f)
			}
		}
	}
	upDir := ""
	if st, err := os.Stat(e.upstreamCheckoutDir(t.ID)); err == nil && st.IsDir() {
		upDir = e.upstreamCheckoutDir(t.ID)
	}
	v := &verifier.Verifier{Cfg: e.Cfg.Verification, Security: e.Cfg.Security}
	e.logf("verifying %s", t.ID)
	rep := v.Run(ctx, verifier.Input{
		WorkDir: t.Worktree, Commit: head, BaseRef: t.BaseSHA, TestFiles: testFiles, UpstreamDir: upDir,
		NewWorktree: func(ctx context.Context, name, rev string) (string, func(), error) {
			return e.WS.Detached(ctx, t.ID+"-"+name, rev)
		},
		Log: e.logf,
	})
	rep.Tree = tree
	t.Verification = rep
	return rep, nil
}

// Verify runs verification for a claimed task (external agent flow).
func (e *Engine) Verify(ctx context.Context, ref, token, owner string) (*state.VerificationReport, error) {
	t, err := e.Store.FindTask(ref)
	if err != nil {
		return nil, err
	}
	if err := tasks.CheckLease(t, token, owner); err != nil {
		return nil, err
	}
	rep, err := e.VerifyTask(ctx, t, t.Result, true)
	if err != nil {
		return nil, err
	}
	t.AddEvent("verified", rep.Outcome+": "+rep.Summary)
	if err := e.saveTask(t); err != nil {
		return nil, err
	}
	if !rep.Passed(e.Cfg.Verification.AllowPartial) {
		return rep, ErrVerificationFailed
	}
	return rep, nil
}

// Complete records an agent's result, verifies the work, and (if
// configured) opens a pull request. Agent claims are never trusted on their
// own: "implemented" work must pass verification, and "no change needed"
// claims below the confidence threshold go to human review.
func (e *Engine) Complete(ctx context.Context, ref string, o CompleteOptions) (*CompleteResult, error) {
	t, err := e.Store.FindTask(ref)
	if err != nil {
		return nil, err
	}
	if err := tasks.CheckLease(t, o.Token, o.Owner); err != nil {
		return nil, err
	}
	if o.Result == nil {
		return nil, fmt.Errorf("a result is required")
	}
	if err := tasks.ValidateResult(o.Result); err != nil {
		return nil, err
	}
	att := o.Attempt
	if att == nil {
		att = &state.Attempt{Agent: firstNonEmpty(o.Agent, "external"), StartedAt: t.Lease.Acquired}
	}
	att.Number = len(t.Attempts) + 1
	att.Owner = t.Lease.Owner
	att.Result = o.Result
	if att.CostUSD == 0 {
		att.CostUSD = o.Result.CostUSD
	}
	att.FinishedAt = state.Now()
	t.Result = o.Result
	t.CostUSD += att.CostUSD
	res := &CompleteResult{Task: t}
	minConf := e.Cfg.Analysis.MinConfidence

	finish := func(outcome string) (*CompleteResult, error) {
		t.Attempts = append(t.Attempts, *att)
		res.Outcome = outcome
		e.Tasks.ReleaseLocked(ctx, t)
		if err := e.saveTask(t); err != nil {
			return nil, err
		}
		return res, nil
	}

	switch o.Result.Status {
	case tasks.ResultAlreadyImplemented, tasks.ResultNotApplicable:
		att.Outcome = o.Result.Status
		status := state.StatusAlreadyImplemented
		if o.Result.Status == tasks.ResultNotApplicable {
			status = state.StatusNotApplicable
		}
		if o.Result.Confidence > 0 && o.Result.Confidence < minConf {
			t.SetStatus(state.TaskNeedsReview, fmt.Sprintf("agent reported %s with low confidence %.2f", o.Result.Status, o.Result.Confidence))
			res.Message = t.StatusReason
			if err := e.updateChanges(t, func(c *state.Change) {
				c.SetStatus(state.StatusNeedsReview, "agent: "+o.Result.Summary)
			}); err != nil {
				return nil, err
			}
			return finish(OutcomeNeedsReview)
		}
		t.SetStatus(state.TaskDone, o.Result.Status)
		if err := e.updateChanges(t, func(c *state.Change) {
			c.Decisions = append(c.Decisions, state.Decision{At: state.Now(), By: "agent:" + att.Agent, Action: o.Result.Status, Reason: o.Result.Summary})
			c.Agent = att.Agent
			c.SetStatus(status, o.Result.Summary)
		}); err != nil {
			return nil, err
		}
		e.cleanupTask(ctx, t)
		res.Message = "no downstream change required: " + o.Result.Summary
		return finish(OutcomeSettled)

	case tasks.ResultNeedsHuman:
		att.Outcome = "needs_human"
		t.SetStatus(state.TaskNeedsReview, o.Result.Summary)
		if err := e.updateChanges(t, func(c *state.Change) { c.SetStatus(state.StatusNeedsReview, "agent: "+o.Result.Summary) }); err != nil {
			return nil, err
		}
		res.Message = "agent requested human review: " + o.Result.Summary
		return finish(OutcomeNeedsReview)

	case tasks.ResultFailed:
		att.Outcome = "agent_failed"
		att.Error = o.Result.Summary
		return e.failAttempt(ctx, t, att, res, finish)
	}

	// implemented
	if t.Worktree != "" {
		protectPaths(ctx, t.Worktree, t.BaseSHA)
	}
	if err := e.applyMemory(ctx, t, o.Result, att.Agent); err != nil {
		e.logf("warning: could not record agent memory: %v", err)
	}
	rep, err := e.VerifyTask(ctx, t, o.Result, false)
	if errors.Is(err, errNoChanges) {
		att.Outcome = "no_changes"
		att.Error = "agent reported implemented but the workspace has no changes"
		return e.failAttempt(ctx, t, att, res, finish)
	}
	if err != nil {
		return nil, err
	}
	att.Verification = rep
	res.Report = rep
	if !rep.Passed(e.Cfg.Verification.AllowPartial) {
		att.Outcome = "verification_failed"
		att.Error = rep.Summary
		return e.failAttempt(ctx, t, att, res, finish)
	}
	changes, err := e.changesOf(t)
	if err != nil {
		return nil, err
	}
	sha, err := e.squash(ctx, t, changes, o.Result)
	if err != nil {
		return nil, err
	}
	rep.Commit = sha
	att.Commit = sha
	att.Outcome = "implemented"
	t.Commit = sha
	t.SetStatus(state.TaskVerified, rep.Outcome)
	if err := e.updateChanges(t, func(c *state.Change) {
		c.DownstreamCommit, c.Branch, c.Agent = sha, t.Branch, att.Agent
		c.CostUSD += att.CostUSD / float64(len(t.Changes))
		c.SetStatus(state.StatusVerified, rep.Outcome)
	}); err != nil {
		return nil, err
	}
	res.Message = "verified (" + rep.Summary + ")"
	if err := e.saveTask(t); err != nil {
		return nil, err
	}
	if e.Cfg.PullRequests.AutoCreate && e.Cfg.PullRequests.Provider != "none" && e.Cfg.PullRequests.Push {
		pr, err := e.Publish(ctx, t)
		if err != nil {
			e.logf("warning: verified but could not open PR for %s: %v", t.ID, err)
			res.Message += "; PR creation failed: " + err.Error()
			return finish(OutcomeVerified)
		}
		res.PR = pr
		res.Message = fmt.Sprintf("opened PR #%d %s", pr.Number, pr.URL)
		if pr.State == "merged" {
			return finish(OutcomeMerged)
		}
		return finish(OutcomePROpened)
	}
	if e.Cfg.PullRequests.Push && e.Cfg.PullRequests.Provider == "none" && e.Repo.RemoteURL(ctx, e.Cfg.Downstream.Remote) != "" {
		if err := e.pushBranch(ctx, t); err != nil {
			res.Message += "; push failed: " + err.Error()
		}
	}
	res.Message += "; branch " + t.Branch + " is ready for review"
	return finish(OutcomeVerified)
}

func (e *Engine) failAttempt(ctx context.Context, t *state.Task, att *state.Attempt, res *CompleteResult, finish func(string) (*CompleteResult, error)) (*CompleteResult, error) {
	used := len(t.Attempts) + 1 - t.RetryBase
	if used >= e.Cfg.Limits.MaxAttemptsPerChange {
		t.SetStatus(state.TaskFailed, fmt.Sprintf("%s after %d attempt(s): %s", att.Outcome, used, truncate(att.Error, 300)))
		if err := e.updateChanges(t, func(c *state.Change) { c.SetStatus(state.StatusFailed, t.StatusReason) }); err != nil {
			return nil, err
		}
		res.Message = t.StatusReason
		e.cleanupTask(ctx, t)
		return finish(OutcomeFailed)
	}
	// Fresh workspace for the next attempt; the failure is in the context.
	e.cleanupTask(ctx, t)
	t.Verification = nil
	t.SetStatus(state.TaskOpen, fmt.Sprintf("attempt %d %s; will retry", att.Number, att.Outcome))
	if err := e.updateChanges(t, func(c *state.Change) { c.SetStatus(state.StatusPlanned, t.StatusReason) }); err != nil {
		return nil, err
	}
	res.Message = t.StatusReason + ": " + truncate(att.Error, 300)
	return finish(OutcomeRetry)
}

// applyMemory records agent-proposed mappings and notes in the task
// workspace, so they reach the main branch through the reviewed PR.
func (e *Engine) applyMemory(ctx context.Context, t *state.Task, r *state.AgentResult, agent string) error {
	if len(r.ProposedMappings) == 0 && len(r.Notes) == 0 || t.Worktree == "" {
		return nil
	}
	dir := filepath.Join(t.Worktree, ".undrift", "memory")
	mem, err := memory.Load(dir)
	if err != nil {
		return err
	}
	for _, p := range r.ProposedMappings {
		if len(p.Upstream) == 0 {
			continue
		}
		mem.UpsertMapping(memory.Mapping{
			Kind: p.Kind, Upstream: p.Upstream, Downstream: p.Downstream, Relationship: p.Relationship,
			Status: p.Status, Confidence: r.Confidence, Source: "agent",
			Notes: "proposed by " + agent + " while synchronizing " + strings.Join(t.Changes, ","),
		})
	}
	for _, n := range r.Notes {
		mem.AddNote(memory.Note{Kind: "learned", Text: n, Source: "agent:" + agent + " (" + t.ID + ")"})
	}
	return mem.Save()
}

func (e *Engine) commitMessage(t *state.Task, changes []*state.Change, r *state.AgentResult) string {
	var b strings.Builder
	if r != nil && strings.TrimSpace(r.CommitMessage) != "" {
		b.WriteString(strings.TrimSpace(r.CommitMessage))
	} else {
		b.WriteString(strings.TrimSpace(pullrequest.Title(e.Cfg.PullRequests.TitlePrefix, changes)))
		if r != nil && r.Summary != "" {
			b.WriteString("\n\n" + r.Summary)
		}
	}
	b.WriteString("\n")
	for i, c := range changes {
		if i == 0 {
			b.WriteString("\n")
		}
		ref := c.SHA
		if u := upstream.CommitURL(c.UpstreamRepo, c.SHA); u != "" {
			ref = u
		}
		fmt.Fprintf(&b, "Upstream-Change: %s %s\n", c.ID, ref)
	}
	fmt.Fprintf(&b, "Undrift-Task: %s\n", t.ID)
	return b.String()
}

// squash collapses the task's commits into one with a descriptive message.
func (e *Engine) squash(ctx context.Context, t *state.Task, changes []*state.Change, r *state.AgentResult) (string, error) {
	wt := git.Open(t.Worktree)
	if _, err := wt.Run(ctx, "reset", "--soft", t.BaseSHA); err != nil {
		return "", err
	}
	sha, committed, err := workspace.CommitAll(ctx, t.Worktree, e.commitMessage(t, changes, r))
	if err != nil {
		return "", err
	}
	if !committed {
		return "", errNoChanges
	}
	return sha, nil
}

func (e *Engine) pushBranch(ctx context.Context, t *state.Task) error {
	_, err := e.Repo.Run(ctx, "push", "--quiet", "--force", e.Cfg.Downstream.Remote, t.Commit+":refs/heads/"+t.Branch)
	return err
}

// Publish pushes a verified task's branch and creates or updates its pull
// request. Calling it repeatedly is safe: an existing PR for the branch is
// updated instead of duplicated.
func (e *Engine) Publish(ctx context.Context, t *state.Task) (*state.PRRef, error) {
	if t.Commit == "" {
		return nil, fmt.Errorf("task %s has no verified commit", t.ID)
	}
	p, err := e.Provider(ctx)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("pull requests are disabled (pull_requests.provider: none)")
	}
	changes, err := e.changesOf(t)
	if err != nil {
		return nil, err
	}
	if err := e.pushBranch(ctx, t); err != nil {
		return nil, err
	}
	mem, _ := e.Memory()
	var devs []memory.Deviation
	var paths []string
	for _, c := range changes {
		paths = append(paths, c.Paths()...)
	}
	if mem != nil {
		devs = mem.DeviationsFor(paths, nil)
	}
	var deps []*state.Change
	for _, d := range t.DependsOn {
		if c, err := e.Store.GetChange(d); err == nil {
			deps = append(deps, c)
		}
	}
	draft := t.Verification != nil && t.Verification.Outcome != state.OutcomeVerified && e.Cfg.PullRequests.DraftOnPartial
	agent := ""
	if n := len(t.Attempts); n > 0 {
		agent = t.Attempts[n-1].Agent
	}
	base := e.Cfg.Downstream.Branch
	if t.StackedOn != "" {
		if dep, err := e.Store.GetTask(t.StackedOn); err == nil && dep.Status != state.TaskDone {
			base = dep.Branch
		}
	}
	req := pullrequest.Request{
		Title: pullrequest.Title(e.Cfg.PullRequests.TitlePrefix, changes),
		Body: pullrequest.Body(pullrequest.BodyInput{
			Task: t, Changes: changes, Result: t.Result, Report: t.Verification,
			Deviations: devs, Dependencies: deps, Agent: agent, Draft: draft,
		}),
		Head: t.Branch, Base: base, Draft: draft, Labels: e.Cfg.PullRequests.Labels,
	}
	existing, err := p.FindByHead(ctx, t.Branch)
	if err != nil {
		return nil, err
	}
	var pr *state.PRRef
	switch {
	case existing != nil && existing.State == "open":
		pr, err = p.Update(ctx, existing.Number, req)
		t.AddEvent("pr_updated", fmt.Sprintf("#%d", existing.Number))
	default:
		pr, err = p.Create(ctx, req)
		if err == nil {
			t.AddEvent("pr_opened", fmt.Sprintf("#%d %s", pr.Number, pr.URL))
		}
	}
	if err != nil {
		return nil, err
	}
	if e.Cfg.PullRequests.AutoMerge && !draft && t.Verification != nil && t.Verification.Outcome == state.OutcomeVerified {
		if err := p.Merge(ctx, pr.Number); err != nil {
			t.AddEvent("auto_merge_failed", err.Error())
		} else {
			pr.State = "merged"
		}
	}
	t.PR = pr
	if pr.State == "merged" {
		t.SetStatus(state.TaskDone, "merged")
	} else {
		t.SetStatus(state.TaskPROpened, fmt.Sprintf("#%d", pr.Number))
	}
	status := state.StatusPROpened
	if pr.State == "merged" {
		status = state.StatusMerged
	}
	if err := e.updateChanges(t, func(c *state.Change) {
		c.PR = pr
		c.SetStatus(status, pr.URL)
	}); err != nil {
		return nil, err
	}
	if pr.State == "merged" {
		e.cleanupTask(ctx, t)
	}
	return pr, e.saveTask(t)
}

// Transition is a state change detected by reconciliation.
type Transition struct {
	Task   string `json:"task"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// Reconcile detects merged or closed pull requests (and, without a
// provider, branches merged into the downstream branch) and finalizes the
// corresponding changes. A change is synchronized only once merged.
func (e *Engine) Reconcile(ctx context.Context) ([]Transition, error) {
	_, _, ts, err := e.Tasks.Snapshot()
	if err != nil {
		return nil, err
	}
	var out []Transition
	base, _ := e.baseRef(ctx)
	for _, t := range ts {
		if t.Status != state.TaskPROpened && t.Status != state.TaskVerified {
			continue
		}
		var to state.TaskStatus
		var reason string
		var pr *state.PRRef
		if t.Status == state.TaskPROpened && t.PR != nil {
			p, err := e.Provider(ctx)
			if err != nil || p == nil {
				continue
			}
			got, err := p.Get(ctx, t.PR.Number)
			if err != nil {
				e.logf("warning: cannot check PR #%d: %v", t.PR.Number, err)
				continue
			}
			pr = got
			switch got.State {
			case "merged":
				to, reason = state.TaskDone, fmt.Sprintf("PR #%d merged", got.Number)
			case "closed":
				to, reason = state.TaskNeedsReview, fmt.Sprintf("PR #%d closed without merging", got.Number)
			}
		} else if t.Commit != "" && base != "" {
			if ok, _ := e.Repo.IsAncestor(ctx, t.Commit, base); ok {
				to, reason = state.TaskDone, "commit merged into "+e.Cfg.Downstream.Branch
			}
		}
		if to == "" {
			continue
		}
		from := t.Status
		t.SetStatus(to, reason)
		if pr != nil {
			t.PR = pr
		}
		cs := state.StatusMerged
		if to == state.TaskNeedsReview {
			cs = state.StatusNeedsReview
		}
		if err := e.updateChanges(t, func(c *state.Change) {
			if pr != nil {
				c.PR = pr
			}
			c.SetStatus(cs, reason)
		}); err != nil {
			return out, err
		}
		if to == state.TaskDone {
			e.cleanupTask(ctx, t)
		}
		if err := e.saveTask(t); err != nil {
			return out, err
		}
		out = append(out, Transition{Task: t.ID, From: string(from), To: string(to), Reason: reason})
	}
	if len(out) > 0 {
		_ = e.Store.Lock(e.refreshLocked)
	}
	return out, nil
}

// leaseRenewer extends a lease periodically until stopped. It cancels the
// work if the lease is lost (e.g. another machine took over).
func (e *Engine) leaseRenewer(ctx context.Context, cancel context.CancelFunc, t *state.Task) func() {
	dur := e.Cfg.State.LeaseDuration.D()
	every := dur / 3
	if every < time.Second {
		every = time.Second
	}
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(every)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tk.C:
				if _, err := e.Tasks.Renew(ctx, t.ID, t.Lease.Token, "", dur); err != nil {
					e.logf("lease for %s lost: %v; cancelling", t.ID, err)
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(stop) }
}
