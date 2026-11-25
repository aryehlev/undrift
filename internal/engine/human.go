package engine

import (
	"context"
	"fmt"
	"os"
	"os/user"

	"github.com/aryehlev/undrift/internal/state"
)

// Actor identifies who made a human decision.
func Actor() string {
	for _, k := range []string{"UNDRIFT_ACTOR", "GITHUB_ACTOR", "USER"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "human"
}

// Decide applies a human decision to a change: skip, approve, or resolve
// to an explicit status. Decisions are recorded on the change permanently
// and are shown to agents working on related changes.
func (e *Engine) Decide(ctx context.Context, ref string, action string, status state.Status, reason string) (*state.Change, error) {
	var out *state.Change
	err := e.Store.Lock(func() error {
		c, err := e.Store.FindChange(ref)
		if err != nil {
			return err
		}
		c.Decisions = append(c.Decisions, state.Decision{At: state.Now(), By: Actor(), Action: action, Reason: reason})
		c.SetStatus(status, reason)
		if err := e.Store.PutChange(c); err != nil {
			return err
		}
		if c.TaskID != "" {
			t, err := e.Store.GetTask(c.TaskID)
			if err == nil {
				if err := e.syncTaskAfterDecision(ctx, t, status); err != nil {
					return err
				}
			}
		}
		out = c
		return e.refreshLocked()
	})
	return out, err
}

// syncTaskAfterDecision keeps a task consistent with its changes.
func (e *Engine) syncTaskAfterDecision(ctx context.Context, t *state.Task, status state.Status) error {
	if status == state.StatusAnalyzed {
		// Approved: reopen the task if it was parked.
		if t.Status == state.TaskNeedsReview || t.Status == state.TaskFailed || t.Status == state.TaskCancelled {
			t.RetryBase = len(t.Attempts)
			t.SetStatus(state.TaskOpen, "approved by "+Actor())
			for _, id := range t.Changes {
				if c, err := e.Store.GetChange(id); err == nil && !c.Status.Settled() {
					c.SetStatus(state.StatusPlanned, "")
					_ = e.Store.PutChange(c)
				}
			}
		}
		return e.Store.PutTask(t)
	}
	allSettled := true
	for _, id := range t.Changes {
		c, err := e.Store.GetChange(id)
		if err != nil {
			return err
		}
		if !c.Status.Settled() {
			allSettled = false
		}
	}
	if allSettled && !t.Status.Terminal() {
		if t.Lease != nil {
			e.Tasks.ReleaseLocked(ctx, t)
		}
		if status == state.StatusMerged {
			t.SetStatus(state.TaskDone, "resolved manually")
		} else {
			t.SetStatus(state.TaskCancelled, "all changes settled by human decision")
		}
		e.cleanupTask(ctx, t)
	}
	return e.Store.PutTask(t)
}

// Skip marks a change as intentionally skipped.
func (e *Engine) Skip(ctx context.Context, ref, reason string) (*state.Change, error) {
	if reason == "" {
		return nil, fmt.Errorf("--reason is required")
	}
	return e.Decide(ctx, ref, "skip", state.StatusSkipped, reason)
}

// Approve sends a change (e.g. one awaiting review or skipped by policy)
// to implementation.
func (e *Engine) Approve(ctx context.Context, ref, reason string) (*state.Change, error) {
	c, err := e.Store.FindChange(ref)
	if err != nil {
		return nil, err
	}
	if c.Status == state.StatusMerged {
		return nil, fmt.Errorf("change %s is already merged", c.ID)
	}
	return e.Decide(ctx, c.ID, "approve", state.StatusAnalyzed, reason)
}

// Resolve records an explicit outcome (already_implemented, not_applicable,
// merged when ported manually).
func (e *Engine) Resolve(ctx context.Context, ref string, status state.Status, reason string) (*state.Change, error) {
	switch status {
	case state.StatusAlreadyImplemented, state.StatusNotApplicable, state.StatusMerged, state.StatusSkipped:
	default:
		return nil, fmt.Errorf("cannot resolve to %q (use already_implemented, not_applicable, merged or skipped)", status)
	}
	return e.Decide(ctx, ref, "resolve:"+string(status), status, reason)
}

// Retry resets a failed or parked task (or re-analyzes an unplanned
// change) so it will be attempted again with a fresh attempt budget.
func (e *Engine) Retry(ctx context.Context, ref string) (*state.Task, error) {
	var out *state.Task
	err := e.Store.Lock(func() error {
		t, err := e.Store.FindTask(ref)
		if err != nil {
			c, cerr := e.Store.FindChange(ref)
			if cerr != nil {
				return err
			}
			if c.TaskID == "" {
				c.Decisions = append(c.Decisions, state.Decision{At: state.Now(), By: Actor(), Action: "retry", Reason: "re-analyze"})
				c.SetStatus(state.StatusDiscovered, "re-analysis requested")
				return e.Store.PutChange(c)
			}
			return err
		}
		switch t.Status {
		case state.TaskFailed, state.TaskNeedsReview, state.TaskCancelled, state.TaskOpen:
		case state.TaskClaimed, state.TaskImplemented:
			if !t.Lease.Expired(timeNow()) {
				return fmt.Errorf("task %s is currently leased by %s", t.ID, t.Lease.Owner)
			}
		default:
			return fmt.Errorf("task %s is %s; nothing to retry", t.ID, t.Status)
		}
		e.Tasks.ReleaseLocked(ctx, t)
		t.RetryBase = len(t.Attempts)
		t.Verification = nil
		t.SetStatus(state.TaskOpen, "retry requested by "+Actor())
		for _, id := range t.Changes {
			c, err := e.Store.GetChange(id)
			if err != nil {
				return err
			}
			if !c.Status.Settled() || c.Status == state.StatusSkipped {
				c.Decisions = append(c.Decisions, state.Decision{At: state.Now(), By: Actor(), Action: "retry"})
				c.SetStatus(state.StatusPlanned, "")
				if err := e.Store.PutChange(c); err != nil {
					return err
				}
			}
		}
		out = t
		if err := e.Store.PutTask(t); err != nil {
			return err
		}
		return e.refreshLocked()
	})
	return out, err
}
