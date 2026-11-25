package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aryehlev/undrift/internal/agents"
	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/tasks"
)

// SyncOptions configures managed execution.
type SyncOptions struct {
	Agent      string
	MaxChanges int
	Parallel   int
	Change     string // only the task containing this change
	Owner      string
	MaxCostUSD float64
}

// TaskOutcome summarizes one processed task.
type TaskOutcome struct {
	Task    string       `json:"task"`
	Changes []string     `json:"changes"`
	Outcome string       `json:"outcome"`
	Message string       `json:"message"`
	PR      *state.PRRef `json:"pr,omitempty"`
	CostUSD float64      `json:"cost_usd,omitempty"`
}

// SyncResult aggregates a sync run.
type SyncResult struct {
	Agent      string        `json:"agent"`
	Processed  []TaskOutcome `json:"processed"`
	Reconciled []Transition  `json:"reconciled,omitempty"`
	CostUSD    float64       `json:"cost_usd"`
	Stopped    string        `json:"stopped,omitempty"`
}

// Sync runs the configured agent on ready tasks until no work remains or
// a limit (changes per run, cost budget) is reached. Independent tasks run
// concurrently up to the parallelism limit, each in its own worktree.
func (e *Engine) Sync(ctx context.Context, o SyncOptions) (*SyncResult, error) {
	if o.Agent == "" {
		o.Agent = e.Cfg.Agent.Provider
	}
	ag, err := agents.New(o.Agent, e.Cfg)
	if err != nil {
		return nil, err
	}
	if o.MaxChanges <= 0 {
		o.MaxChanges = e.Cfg.Limits.MaxChangesPerRun
	}
	if o.Parallel <= 0 {
		o.Parallel = e.Cfg.Agent.MaxParallelTasks
	}
	if o.MaxCostUSD <= 0 {
		o.MaxCostUSD = e.Cfg.Limits.MaxCostUSD
	}
	if o.Owner == "" {
		o.Owner = tasks.Owner()
	}
	res := &SyncResult{Agent: ag.Name()}
	if tr, err := e.Reconcile(ctx); err == nil {
		res.Reconciled = tr
	}
	if _, err := e.Plan(ctx); err != nil {
		return nil, err
	}

	var filter func(*state.Task) bool
	if o.Change != "" {
		t, err := e.Store.FindTask(o.Change)
		if err != nil {
			return nil, fmt.Errorf("no task for %q (has it been analyzed and planned?): %w", o.Change, err)
		}
		id := t.ID
		filter = func(t *state.Task) bool { return t.ID == id }
		o.MaxChanges = len(t.Changes)
	}

	var (
		mu      sync.Mutex
		claimed int
		wg      sync.WaitGroup
		stopped string
	)
	budgetOK := func(n int) bool {
		mu.Lock()
		defer mu.Unlock()
		if claimed+n > o.MaxChanges && claimed > 0 {
			stopped = fmt.Sprintf("max_changes_per_run (%d) reached", o.MaxChanges)
			return false
		}
		if o.MaxCostUSD > 0 && res.CostUSD >= o.MaxCostUSD {
			stopped = fmt.Sprintf("cost budget $%.2f reached", o.MaxCostUSD)
			return false
		}
		claimed += n
		return true
	}
	worker := func() {
		defer wg.Done()
		for ctx.Err() == nil {
			mu.Lock()
			full := claimed >= o.MaxChanges
			mu.Unlock()
			if full {
				return
			}
			t, err := e.Tasks.ClaimNext(ctx, o.Owner, 0, filter)
			if err != nil {
				e.logf("claim failed: %v", err)
				return
			}
			if t == nil {
				return
			}
			if !budgetOK(len(t.Changes)) {
				_, _ = e.Tasks.Release(ctx, t.ID, t.Lease.Token, "")
				return
			}
			out := e.runTask(ctx, ag, t)
			mu.Lock()
			res.Processed = append(res.Processed, out)
			res.CostUSD += out.CostUSD
			mu.Unlock()
			// New work may have become ready (dependents of this task).
			_, _ = e.Plan(ctx)
		}
	}
	for i := 0; i < o.Parallel; i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	res.Stopped = stopped
	if ctx.Err() != nil {
		res.Stopped = "interrupted"
	}
	if len(res.Processed) == 0 && res.Stopped == "" && len(res.Reconciled) == 0 {
		return res, ErrNothingToDo
	}
	return res, nil
}

// runTask executes one task with the agent and completes it.
func (e *Engine) runTask(ctx context.Context, ag agents.Agent, t *state.Task) TaskOutcome {
	out := TaskOutcome{Task: t.ID, Changes: t.Changes}
	fail := func(err error) TaskOutcome {
		out.Outcome, out.Message = OutcomeFailed, err.Error()
		e.logf("task %s: %v", t.ID, err)
		// Return the task to the pool; the lease is released so it can be retried.
		_, _ = e.Tasks.Release(context.Background(), t.ID, t.Lease.Token, "")
		return out
	}
	cr, err := e.prepare(ctx, t)
	if err != nil {
		return fail(fmt.Errorf("prepare workspace: %w", err))
	}
	t = cr.Task
	prompt, err := os.ReadFile(cr.PromptFile)
	if err != nil {
		return fail(err)
	}
	n := len(t.Attempts) + 1
	logFile := filepath.Join(e.TaskDir(t.ID), fmt.Sprintf("attempt-%d.log", n))
	lf, err := os.Create(logFile)
	if err != nil {
		return fail(err)
	}
	defer lf.Close()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopRenew := e.leaseRenewer(runCtx, cancel, t)
	defer stopRenew()

	e.logf("task %s: running %s (attempt %d) in %s", t.ID, ag.Name(), n, cr.Workdir)
	att := &state.Attempt{Agent: ag.Name(), StartedAt: state.Now(), LogFile: logFile}
	resp, err := ag.Run(runCtx, agents.Request{
		Mode: agents.ModeImplement, Prompt: string(prompt), PromptFile: cr.PromptFile,
		TaskFile: cr.TaskFile, ResultFile: cr.ResultFile, WorkDir: cr.Workdir,
		Timeout: e.Cfg.Agent.Timeout.D(), Model: e.Cfg.Agent.Model, Log: lf,
	})
	result := &state.AgentResult{}
	switch {
	case err != nil:
		result.Status, result.Summary = tasks.ResultFailed, err.Error()
	case resp.TimedOut:
		att.CostUSD = resp.CostUSD
		result.Status, result.Summary = tasks.ResultFailed, fmt.Sprintf("agent timed out after %s", e.Cfg.Agent.Timeout.D())
	default:
		att.CostUSD = resp.CostUSD
		parsed, perr := tasks.ParseResult(resp.ResultJSON)
		switch {
		case perr == nil:
			result = parsed
		case resp.ExitCode != 0 || resp.IsError:
			result.Status = tasks.ResultFailed
			result.Summary = fmt.Sprintf("agent exited with status %d: %s", resp.ExitCode, proc80(resp.Output))
		default:
			// No structured result: treat as implemented with unknown
			// confidence. Verification decides whether it is acceptable.
			result.Status = tasks.ResultImplemented
			result.Summary = "Agent did not produce a structured result. Output tail: " + proc80(resp.Output)
			result.Limitations = []string{"agent did not report a structured result (" + errString(perr) + ")"}
		}
	}
	if runCtx.Err() != nil && ctx.Err() == nil && result.Status != tasks.ResultFailed {
		result.Status, result.Summary = tasks.ResultFailed, "lease lost during execution"
	}
	if ctx.Err() != nil {
		// Interrupted: leave the lease to expire so the work can be resumed.
		out.Outcome, out.Message = "interrupted", "run interrupted; task will be recovered when its lease expires"
		return out
	}
	cres, err := e.Complete(context.Background(), t.ID, CompleteOptions{Token: t.Lease.Token, Result: result, Agent: ag.Name(), Attempt: att})
	if err != nil {
		return fail(fmt.Errorf("complete: %w", err))
	}
	out.Outcome, out.Message, out.PR, out.CostUSD = cres.Outcome, cres.Message, cres.PR, att.CostUSD
	e.logf("task %s: %s — %s", t.ID, out.Outcome, out.Message)
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// RunOptions configures the full pipeline.
type RunOptions struct {
	Sync          SyncOptions
	AnalyzeAgent  string
	AcceptRewrite bool
	SkipSync      bool
}

// RunResult aggregates a pipeline run.
type RunResult struct {
	Scan    *ScanResult    `json:"scan"`
	Analyze *AnalyzeResult `json:"analyze"`
	Plan    *PlanResult    `json:"plan"`
	Sync    *SyncResult    `json:"sync,omitempty"`
}

// Run executes scan → analyze → plan → sync.
func (e *Engine) Run(ctx context.Context, o RunOptions) (*RunResult, error) {
	res := &RunResult{}
	var err error
	if res.Scan, err = e.Scan(ctx, ScanOptions{AcceptRewrite: o.AcceptRewrite}); err != nil {
		return res, err
	}
	if res.Analyze, err = e.Analyze(ctx, AnalyzeOptions{Agent: o.AnalyzeAgent}); err != nil {
		return res, err
	}
	if res.Plan, err = e.Plan(ctx); err != nil {
		return res, err
	}
	if o.SkipSync {
		return res, nil
	}
	res.Sync, err = e.Sync(ctx, o.Sync)
	if errors.Is(err, ErrNothingToDo) {
		err = nil
	}
	return res, err
}

// Watch runs the pipeline repeatedly.
func (e *Engine) Watch(ctx context.Context, interval time.Duration, o RunOptions, report func(*RunResult, error)) error {
	if interval <= 0 {
		interval = e.Cfg.Watch.Interval.D()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	for {
		res, err := e.Run(ctx, o)
		report(res, err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
