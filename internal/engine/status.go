package engine

import (
	"context"
	"sort"
	"time"

	"github.com/aryehlev/undrift/internal/planner"
	"github.com/aryehlev/undrift/internal/state"
)

func timeNow() time.Time { return time.Now() }

// Status summarizes synchronization state.
type Status struct {
	Upstreams []UpstreamStatus         `json:"upstreams"`
	Changes   map[state.Status]int     `json:"changes"`
	Tasks     map[state.TaskStatus]int `json:"tasks"`
	Total     int                      `json:"total_changes"`
	Synced    int                      `json:"synchronized"`
	Remaining int                      `json:"remaining"`
	Attention []ChangeLine             `json:"needs_attention"`
	InFlight  []ChangeLine             `json:"in_flight"`
	OpenPRs   []ChangeLine             `json:"open_prs"`
	CostUSD   float64                  `json:"cost_usd"`
}

// UpstreamStatus is per-upstream scan state.
type UpstreamStatus struct {
	Name        string    `json:"name"`
	Repository  string    `json:"repository"`
	Branch      string    `json:"branch"`
	LastScanned string    `json:"last_scanned,omitempty"`
	ScannedAt   time.Time `json:"scanned_at,omitempty"`
}

// ChangeLine is a one-line change summary.
type ChangeLine struct {
	ID             string `json:"id"`
	Ref            string `json:"ref"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	Classification string `json:"classification,omitempty"`
	Reason         string `json:"reason,omitempty"`
	PR             string `json:"pr,omitempty"`
	Task           string `json:"task,omitempty"`
}

func line(c *state.Change) ChangeLine {
	l := ChangeLine{ID: c.ID, Ref: c.Ref(), Title: firstLine(c.Title), Status: string(c.Status), Reason: c.StatusReason, Task: c.TaskID}
	if c.Analysis != nil {
		l.Classification = string(c.Analysis.Classification)
	}
	if c.PR != nil {
		l.PR = c.PR.URL
	}
	return l
}

// Status computes the project status.
func (e *Engine) Status(ctx context.Context) (*Status, error) {
	meta, err := e.Store.Meta()
	if err != nil {
		return nil, err
	}
	cs, err := e.Store.Changes()
	if err != nil {
		return nil, err
	}
	ts, err := e.Store.Tasks()
	if err != nil {
		return nil, err
	}
	s := &Status{Changes: map[state.Status]int{}, Tasks: map[state.TaskStatus]int{}, Total: len(cs)}
	for _, u := range e.Cfg.Upstreams {
		us := UpstreamStatus{Name: u.Name, Repository: u.Repository, Branch: u.Branch}
		if c := meta.Cursors[u.Name]; c != nil {
			us.LastScanned, us.ScannedAt = c.LastScanned, c.ScannedAt
		}
		s.Upstreams = append(s.Upstreams, us)
	}
	for _, c := range cs {
		s.Changes[c.Status]++
		s.CostUSD += c.CostUSD
		switch {
		case c.Status.Settled():
			s.Synced++
		default:
			s.Remaining++
		}
		switch {
		case c.Status.NeedsAttention():
			s.Attention = append(s.Attention, line(c))
		case c.Status == state.StatusPROpened:
			s.OpenPRs = append(s.OpenPRs, line(c))
		case c.Status == state.StatusInProgress || c.Status == state.StatusImplemented || c.Status == state.StatusVerified:
			s.InFlight = append(s.InFlight, line(c))
		}
	}
	for _, t := range ts {
		s.Tasks[t.Status]++
	}
	return s, nil
}

// Inspection is a detailed view of one change.
type Inspection struct {
	Change    *state.Change      `json:"change"`
	Task      *state.Task        `json:"task,omitempty"`
	Readiness *planner.Readiness `json:"readiness,omitempty"`
}

// Inspect returns a change with its task and readiness.
func (e *Engine) Inspect(ctx context.Context, ref string) (*Inspection, error) {
	c, err := e.Store.FindChange(ref)
	if err != nil {
		t, terr := e.Store.FindTask(ref)
		if terr != nil {
			return nil, err
		}
		if c, err = e.Store.GetChange(t.Changes[0]); err != nil {
			return nil, err
		}
	}
	in := &Inspection{Change: c}
	if c.TaskID != "" {
		if t, err := e.Store.GetTask(c.TaskID); err == nil {
			in.Task = t
			cm, tm, _, err := e.Tasks.Snapshot()
			if err == nil {
				r := planner.Check(t, cm, tm, e.Cfg)
				in.Readiness = &r
			}
		}
	}
	return in, nil
}

// HistoryEntry is one decision or lifecycle event.
type HistoryEntry struct {
	At     time.Time `json:"at"`
	Change string    `json:"change"`
	Title  string    `json:"title"`
	Kind   string    `json:"kind"` // decision | event
	By     string    `json:"by,omitempty"`
	Action string    `json:"action"`
	Detail string    `json:"detail,omitempty"`
}

// History lists decisions (and optionally all events), newest first.
func (e *Engine) History(ctx context.Context, events bool, limit int) ([]HistoryEntry, error) {
	cs, err := e.Store.Changes()
	if err != nil {
		return nil, err
	}
	var out []HistoryEntry
	for _, c := range cs {
		for _, d := range c.Decisions {
			out = append(out, HistoryEntry{At: d.At, Change: c.ID, Title: firstLine(c.Title), Kind: "decision", By: d.By, Action: d.Action, Detail: d.Reason})
		}
		if events {
			for _, ev := range c.Events {
				out = append(out, HistoryEntry{At: ev.At, Change: c.ID, Title: firstLine(c.Title), Kind: "event", Action: ev.Type, Detail: ev.Detail})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Changes lists changes, optionally filtered by status.
func (e *Engine) Changes(ctx context.Context, statuses []string) ([]ChangeLine, error) {
	cs, err := e.Store.Changes()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, s := range statuses {
		want[s] = true
	}
	var out []ChangeLine
	for _, c := range cs {
		if len(want) == 0 || want[string(c.Status)] {
			out = append(out, line(c))
		}
	}
	return out, nil
}
