package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aryehlev/undrift/internal/planner"
	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/tasks"
	"github.com/aryehlev/undrift/internal/workspace"
)

// PlanResult describes the execution plan.
type PlanResult struct {
	Created []PlannedTask `json:"created"`
	Ready   []PlannedTask `json:"ready"`
	Waiting []PlannedTask `json:"waiting"`
	Blocked []PlannedTask `json:"blocked"`
}

// PlannedTask is a task with readiness.
type PlannedTask struct {
	ID        string   `json:"id"`
	Changes   []string `json:"changes"`
	Title     string   `json:"title"`
	Priority  int      `json:"priority"`
	DependsOn []string `json:"depends_on,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	StackOn   string   `json:"stack_on,omitempty"`
}

// Plan creates tasks for analyzed changes and refreshes readiness.
func (e *Engine) Plan(ctx context.Context) (*PlanResult, error) {
	res := &PlanResult{}
	err := e.Store.Lock(func() error {
		all, err := e.Store.Changes()
		if err != nil {
			return err
		}
		p := planner.Plan(all, e.Cfg)
		for _, t := range p.Tasks {
			if existing, err := e.Store.GetTask(t.ID); err == nil {
				// Idempotency: never duplicate a task; re-link its changes.
				t = existing
			} else if err := e.Store.PutTask(t); err != nil {
				return err
			}
			res.Created = append(res.Created, PlannedTask{ID: t.ID, Changes: t.Changes, Priority: t.Priority, DependsOn: t.DependsOn})
		}
		for _, c := range p.Changes {
			if err := e.Store.PutChange(c); err != nil {
				return err
			}
		}
		return e.refreshLocked()
	})
	if err != nil {
		return nil, err
	}
	cm, tm, ts, err := e.Tasks.Snapshot()
	if err != nil {
		return nil, err
	}
	for i := range res.Created {
		if c := cm[res.Created[i].Changes[0]]; c != nil {
			res.Created[i].Title = c.Title
		}
	}
	for _, t := range ts {
		if t.Status != state.TaskOpen {
			continue
		}
		r := planner.Check(t, cm, tm, e.Cfg)
		pt := PlannedTask{ID: t.ID, Changes: t.Changes, Priority: t.Priority, DependsOn: t.DependsOn, Reason: r.Reason, StackOn: r.StackOn}
		if c := cm[t.Changes[0]]; c != nil {
			pt.Title = c.Title
		}
		switch {
		case r.Ready:
			res.Ready = append(res.Ready, pt)
		case r.Blocked:
			res.Blocked = append(res.Blocked, pt)
		default:
			res.Waiting = append(res.Waiting, pt)
		}
	}
	return res, nil
}

// refreshLocked updates change statuses of open tasks from readiness.
func (e *Engine) refreshLocked() error {
	cm, tm, ts, err := e.Tasks.Snapshot()
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.Status != state.TaskOpen {
			continue
		}
		r := planner.Check(t, cm, tm, e.Cfg)
		want := state.StatusPlanned
		switch {
		case r.Blocked:
			want = state.StatusBlocked
		case !r.Ready:
			want = state.StatusWaitingDependency
		}
		for _, id := range t.Changes {
			c := cm[id]
			if c == nil {
				continue
			}
			switch c.Status {
			case state.StatusPlanned, state.StatusWaitingDependency, state.StatusBlocked:
				if c.Status != want || (want != state.StatusPlanned && c.StatusReason != r.Reason) {
					reason := r.Reason
					if want == state.StatusPlanned {
						reason = ""
					}
					c.SetStatus(want, reason)
					if err := e.Store.PutChange(c); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// Next returns ready tasks in execution order without claiming them.
func (e *Engine) Next(ctx context.Context, all bool) ([]tasks.Candidate, error) {
	if _, err := e.Plan(ctx); err != nil {
		return nil, err
	}
	ready, err := e.Tasks.Ready()
	if err != nil {
		return nil, err
	}
	if len(ready) == 0 {
		return nil, ErrNothingToDo
	}
	if !all {
		ready = ready[:1]
	}
	return ready, nil
}

// ClaimResult tells a worker where to do the work.
type ClaimResult struct {
	Task       *state.Task `json:"task"`
	Token      string      `json:"token"`
	Workdir    string      `json:"workdir"`
	TaskFile   string      `json:"task_file"`
	PromptFile string      `json:"prompt_file"`
	ResultFile string      `json:"result_file"`
	ExpiresAt  time.Time   `json:"lease_expires_at"`
}

// Claim leases a task and prepares its isolated workspace and contract.
func (e *Engine) Claim(ctx context.Context, ref, owner string, lease time.Duration, force bool) (*ClaimResult, error) {
	if owner == "" {
		owner = tasks.Owner()
	}
	t, err := e.Tasks.Claim(ctx, ref, owner, lease, force)
	if err != nil {
		return nil, err
	}
	return e.prepare(ctx, t)
}

// baseRef returns the downstream base revision for new work.
func (e *Engine) baseRef(ctx context.Context) (string, error) {
	d := e.Cfg.Downstream
	if e.Repo.RemoteURL(ctx, d.Remote) != "" {
		_, _ = e.Repo.Run(ctx, "fetch", "--quiet", d.Remote, d.Branch)
		if ref := "refs/remotes/" + d.Remote + "/" + d.Branch; e.Repo.HasRev(ctx, ref) {
			return ref, nil
		}
	}
	if e.Repo.HasRev(ctx, "refs/heads/"+d.Branch) {
		return "refs/heads/" + d.Branch, nil
	}
	if e.Repo.HasRev(ctx, "HEAD") {
		return "HEAD", nil
	}
	return "", fmt.Errorf("downstream branch %q not found", d.Branch)
}

// prepare creates the worktree and writes task.json/prompt.md.
func (e *Engine) prepare(ctx context.Context, t *state.Task) (*ClaimResult, error) {
	base, err := e.baseRef(ctx)
	if err != nil {
		return nil, err
	}
	if t.StackedOn != "" {
		dep, err := e.Store.GetTask(t.StackedOn)
		if err != nil {
			return nil, err
		}
		if !e.Repo.HasRev(ctx, "refs/heads/"+dep.Branch) {
			_, _ = e.Repo.Run(ctx, "fetch", "--quiet", e.Cfg.Downstream.Remote, dep.Branch+":"+dep.Branch)
		}
		base = "refs/heads/" + dep.Branch
	}
	dir := e.WS.Path(t.ID)
	reuse := t.Worktree == dir && t.BaseSHA != ""
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		reuse = false
	}
	if !reuse {
		sha, err := e.Repo.RevParse(ctx, base)
		if err != nil {
			return nil, err
		}
		if dir, err = e.WS.Prepare(ctx, t.ID, t.Branch, sha); err != nil {
			return nil, err
		}
		t.Base, t.BaseSHA = base, sha
	} else if _, err := e.WS.Prepare(ctx, t.ID, t.Branch, t.BaseSHA); err != nil {
		return nil, err
	}
	t.Worktree = dir
	if err := e.saveTask(t); err != nil {
		return nil, err
	}
	contract, err := e.BuildContract(ctx, t)
	if err != nil {
		return nil, err
	}
	cr, err := e.writeContract(t, contract)
	if err != nil {
		return nil, err
	}
	cr.Token = t.Lease.Token
	cr.ExpiresAt = t.Lease.ExpiresAt
	return cr, nil
}

func (e *Engine) writeContract(t *state.Task, c *tasks.Contract) (*ClaimResult, error) {
	td := e.TaskDir(t.ID)
	if err := os.MkdirAll(td, 0o755); err != nil {
		return nil, err
	}
	cr := &ClaimResult{
		Task:       t,
		Workdir:    t.Worktree,
		TaskFile:   filepath.Join(td, "task.json"),
		PromptFile: filepath.Join(td, "prompt.md"),
		ResultFile: c.Output.ResultFile,
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := state.WriteFileAtomic(cr.TaskFile, data); err != nil {
		return nil, err
	}
	if err := state.WriteFileAtomic(cr.PromptFile, []byte(tasks.RenderPrompt(c))); err != nil {
		return nil, err
	}
	return cr, nil
}

// cleanupTask removes a task's worktrees once no more work is expected.
func (e *Engine) cleanupTask(ctx context.Context, t *state.Task) {
	if t.Worktree != "" {
		_ = e.WS.Remove(ctx, t.Worktree)
		t.Worktree = ""
	}
	for _, id := range t.Changes {
		if c, err := e.Store.GetChange(id); err == nil {
			if w, err := e.Watcher(c.Upstream); err == nil {
				workspace.RemoveFrom(ctx, w.Mirror, e.upstreamCheckoutDir(t.ID))
				break
			}
		}
	}
}

func (e *Engine) upstreamCheckoutDir(taskID string) string {
	return filepath.Join(e.Cfg.RunDir(), "upstream-checkouts", taskID)
}

func yamlString(v any) string {
	out, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(out)
}

func getenvAny(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}
