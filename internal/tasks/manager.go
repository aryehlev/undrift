// Package tasks manages synchronization tasks: selection of ready work,
// claiming with renewable leases, crash recovery, and the task contract
// handed to coding agents.
package tasks

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/planner"
	"github.com/aryehlev/undrift/internal/state"
)

// Manager coordinates task ownership.
type Manager struct {
	Store  *state.Store
	Cfg    *config.Config
	Leases LeaseBackend
}

// Candidate is a task with its readiness evaluation.
type Candidate struct {
	Task      *state.Task       `json:"task"`
	Readiness planner.Readiness `json:"readiness"`
}

// Snapshot loads all changes and tasks indexed by ID.
func (m *Manager) Snapshot() (map[string]*state.Change, map[string]*state.Task, []*state.Task, error) {
	cs, err := m.Store.Changes()
	if err != nil {
		return nil, nil, nil, err
	}
	ts, err := m.Store.Tasks()
	if err != nil {
		return nil, nil, nil, err
	}
	cm := make(map[string]*state.Change, len(cs))
	for _, c := range cs {
		cm[c.ID] = c
	}
	tm := make(map[string]*state.Task, len(ts))
	for _, t := range ts {
		tm[t.ID] = t
	}
	return cm, tm, ts, nil
}

// Claimable reports whether a worker may take the task now (ignoring
// dependencies). Tasks whose lease expired mid-flight are claimable again,
// which is how crashed workers are recovered.
func (m *Manager) Claimable(t *state.Task, now time.Time) (bool, string) {
	switch t.Status {
	case state.TaskOpen:
		if t.Lease != nil && !t.Lease.Expired(now) {
			return false, "leased by " + t.Lease.Owner
		}
		if n := len(t.Attempts) - t.RetryBase; n >= m.Cfg.Limits.MaxAttemptsPerChange {
			return false, fmt.Sprintf("attempt limit reached (%d)", n)
		}
		return true, ""
	case state.TaskClaimed, state.TaskImplemented:
		if t.Lease.Expired(now) {
			return true, "lease expired; recovering"
		}
		return false, "leased by " + t.Lease.Owner
	}
	return false, "status " + string(t.Status)
}

// Ready lists claimable tasks whose dependencies are satisfied, in
// priority order.
func (m *Manager) Ready() ([]Candidate, error) {
	cm, tm, ts, err := m.Snapshot()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []Candidate
	for _, t := range ts {
		if ok, _ := m.Claimable(t, now); !ok {
			continue
		}
		r := planner.Check(t, cm, tm, m.Cfg)
		if r.Ready {
			out = append(out, Candidate{Task: t, Readiness: r})
		}
	}
	return out, nil
}

// Owner returns a default worker identity.
func Owner() string {
	if v := os.Getenv("UNDRIFT_WORKER"); v != "" {
		return v
	}
	h, _ := os.Hostname()
	return fmt.Sprintf("%s:%d", h, os.Getpid())
}

// Claim leases a task to owner. It fails if the task is not claimable,
// its dependencies are not satisfied (unless force), or another worker
// holds the lease.
func (m *Manager) Claim(ctx context.Context, id, owner string, dur time.Duration, force bool) (*state.Task, error) {
	var out *state.Task
	err := m.Store.Lock(func() error {
		cm, tm, _, err := m.Snapshot()
		if err != nil {
			return err
		}
		t, err := m.Store.FindTask(id)
		if err != nil {
			return err
		}
		t = tm[t.ID]
		now := time.Now()
		if ok, why := m.Claimable(t, now); !ok {
			if t.Lease != nil && !t.Lease.Expired(now) && t.Lease.Owner != owner {
				return fmt.Errorf("%w: %s", ErrLeaseHeld, why)
			}
			if !(t.Lease != nil && t.Lease.Owner == owner) {
				return fmt.Errorf("task %s is not claimable: %s", t.ID, why)
			}
		}
		r := planner.Check(t, cm, tm, m.Cfg)
		if !r.Ready && !force {
			return fmt.Errorf("task %s is not ready: %s", t.ID, r.Reason)
		}
		if err := m.claimLocked(ctx, t, cm, owner, dur, r); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

// ClaimNext atomically selects and leases the next ready task. It returns
// nil if no task is ready. filter may restrict the candidates.
func (m *Manager) ClaimNext(ctx context.Context, owner string, dur time.Duration, filter func(*state.Task) bool) (*state.Task, error) {
	var out *state.Task
	err := m.Store.Lock(func() error {
		cm, tm, ts, err := m.Snapshot()
		if err != nil {
			return err
		}
		now := time.Now()
		for _, t := range ts {
			if ok, _ := m.Claimable(t, now); !ok {
				continue
			}
			if filter != nil && !filter(t) {
				continue
			}
			r := planner.Check(t, cm, tm, m.Cfg)
			if !r.Ready {
				continue
			}
			if err := m.claimLocked(ctx, t, cm, owner, dur, r); err != nil {
				if isLeaseHeld(err) {
					continue // another machine got it first
				}
				return err
			}
			out = t
			return nil
		}
		return nil
	})
	return out, err
}

func (m *Manager) claimLocked(ctx context.Context, t *state.Task, cm map[string]*state.Change, owner string, dur time.Duration, r planner.Readiness) error {
	if dur <= 0 {
		dur = m.Cfg.State.LeaseDuration.D()
	}
	prev := t.Lease
	l := &state.Lease{
		Owner:     owner,
		Token:     NewToken(),
		Acquired:  state.Now(),
		ExpiresAt: time.Now().Add(dur).UTC(),
		Backend:   m.Leases.Name(),
	}
	if prev != nil && prev.Owner == owner && !prev.Expired(time.Now()) {
		l.Token = prev.Token // idempotent re-claim by the same owner
	}
	if err := m.Leases.Acquire(ctx, t.ID, l, prev); err != nil {
		return err
	}
	recovering := t.Status == state.TaskClaimed || t.Status == state.TaskImplemented
	t.Lease = l
	t.StackedOn = r.StackOn
	if t.Status == state.TaskOpen {
		t.SetStatus(state.TaskClaimed, "claimed by "+owner)
	} else if recovering {
		t.AddEvent("recovered", "lease expired; reclaimed by "+owner)
	}
	if err := m.Store.PutTask(t); err != nil {
		return err
	}
	for _, id := range t.Changes {
		if c := cm[id]; c != nil && !c.Status.Settled() {
			c.SetStatus(state.StatusInProgress, "task "+t.ID+" claimed by "+owner)
			if err := m.Store.PutChange(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// CheckLease verifies that token (or owner) holds t's lease.
func CheckLease(t *state.Task, token, owner string) error {
	if t.Lease == nil {
		return fmt.Errorf("task %s is not claimed (run `undrift claim %s`)", t.ID, t.ID)
	}
	if token != "" && t.Lease.Token == token {
		return nil
	}
	if token == "" && owner != "" && t.Lease.Owner == owner {
		return nil
	}
	if t.Lease.Expired(time.Now()) && token == "" {
		return nil // expired leases may be resumed by anyone
	}
	return fmt.Errorf("%w (owner %s)", ErrLeaseHeld, t.Lease.Owner)
}

// Renew extends a lease.
func (m *Manager) Renew(ctx context.Context, id, token, owner string, dur time.Duration) (*state.Task, error) {
	if dur <= 0 {
		dur = m.Cfg.State.LeaseDuration.D()
	}
	var out *state.Task
	err := m.Store.Lock(func() error {
		t, err := m.Store.FindTask(id)
		if err != nil {
			return err
		}
		if err := CheckLease(t, token, owner); err != nil {
			return err
		}
		t.Lease.ExpiresAt = time.Now().Add(dur).UTC()
		if err := m.Leases.Renew(ctx, t.ID, t.Lease); err != nil {
			return err
		}
		out = t
		return m.Store.PutTask(t)
	})
	return out, err
}

// Release drops a lease, returning the task to the open pool if it was
// merely claimed.
func (m *Manager) Release(ctx context.Context, id, token, owner string) (*state.Task, error) {
	var out *state.Task
	err := m.Store.Lock(func() error {
		t, err := m.Store.FindTask(id)
		if err != nil {
			return err
		}
		if t.Lease == nil {
			out = t
			return nil
		}
		if err := CheckLease(t, token, owner); err != nil {
			return err
		}
		if err := m.Leases.Release(ctx, t.ID, t.Lease); err != nil {
			return err
		}
		t.Lease = nil
		if t.Status == state.TaskClaimed {
			t.SetStatus(state.TaskOpen, "released")
			if err := m.resetChanges(t, state.StatusPlanned, ""); err != nil {
				return err
			}
		}
		out = t
		return m.Store.PutTask(t)
	})
	return out, err
}

// ReleaseLocked drops the lease of t; the caller holds the store lock and
// saves t.
func (m *Manager) ReleaseLocked(ctx context.Context, t *state.Task) {
	if t.Lease != nil {
		_ = m.Leases.Release(ctx, t.ID, t.Lease)
		t.Lease = nil
	}
}

func (m *Manager) resetChanges(t *state.Task, s state.Status, reason string) error {
	for _, id := range t.Changes {
		c, err := m.Store.GetChange(id)
		if err != nil {
			return err
		}
		if !c.Status.Settled() {
			c.SetStatus(s, reason)
			if err := m.Store.PutChange(c); err != nil {
				return err
			}
		}
	}
	return nil
}

func isLeaseHeld(err error) bool {
	for e := err; e != nil; {
		if e == ErrLeaseHeld {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
