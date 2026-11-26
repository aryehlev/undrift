// Package planner turns analyzed changes into an execution plan: a set of
// tasks with dependencies between them.
//
// Dependencies come from three sources:
//
//  1. explicit: dependencies reported by semantic analysis;
//  2. reverts: a revert depends on the change it reverts;
//  3. file overlap: a change depends on the nearest earlier unsettled change
//     that touched any of the same files (so A → B → C chains preserve
//     upstream order where it matters, while unrelated changes stay
//     independent and can run concurrently).
//
// Task dependencies are expressed as change IDs, so a task can depend on a
// change that is still awaiting human review.
package planner

import (
	"sort"
	"strings"

	"github.com/aryehlev/undrift/internal/analyzer"
	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/state"
)

// Result is the outcome of planning.
type Result struct {
	Tasks   []*state.Task   // newly created tasks
	Changes []*state.Change // changes that were assigned to tasks
}

// active reports whether an earlier change can still affect later ones
// (i.e. it is neither settled nor merely discovered).
func active(s state.Status) bool {
	if s.Settled() {
		return false
	}
	switch s {
	case state.StatusDiscovered, state.StatusPending:
		return false
	}
	return true
}

// Plan creates tasks for analyzed changes that have none.
func Plan(all []*state.Change, cfg *config.Config) *Result {
	byID := map[string]*state.Change{}
	byUpstream := map[string][]*state.Change{}
	for _, c := range all {
		byID[c.ID] = c
		byUpstream[c.Upstream] = append(byUpstream[c.Upstream], c)
	}
	for _, cs := range byUpstream {
		sort.Slice(cs, func(i, j int) bool { return cs[i].Seq < cs[j].Seq })
	}

	var candidates []*state.Change
	for _, c := range all {
		if c.Status == state.StatusAnalyzed && c.TaskID == "" {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return &Result{}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Upstream != candidates[j].Upstream {
			return candidates[i].Upstream < candidates[j].Upstream
		}
		return candidates[i].Seq < candidates[j].Seq
	})

	deps := map[string][]string{}
	for _, c := range candidates {
		deps[c.ID] = dependencies(c, byUpstream[c.Upstream], cfg.Planner.Granularity == "symbol")
	}

	// Group dependent chains into single tasks when configured.
	parent := map[string]string{}
	size := map[string]int{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	isCandidate := map[string]bool{}
	for _, c := range candidates {
		isCandidate[c.ID] = true
		parent[c.ID] = c.ID
		size[c.ID] = 1
	}
	if cfg.Planner.GroupDependent {
		for _, c := range candidates {
			for _, d := range deps[c.ID] {
				if !isCandidate[d] {
					continue
				}
				a, b := find(c.ID), find(d)
				if a != b && size[a]+size[b] <= cfg.Planner.MaxGroupSize {
					parent[a] = b
					size[b] += size[a]
				}
			}
		}
	}
	groups := map[string][]*state.Change{}
	var order []string
	for _, c := range candidates {
		r := find(c.ID)
		if _, ok := groups[r]; !ok {
			order = append(order, r)
		}
		groups[r] = append(groups[r], c)
	}

	res := &Result{}
	for _, r := range order {
		members := groups[r]
		in := map[string]bool{}
		for _, m := range members {
			in[m.ID] = true
		}
		first := members[0]
		t := &state.Task{
			ID:       "t-" + first.ID,
			Status:   state.TaskOpen,
			Branch:   cfg.PullRequests.BranchPrefix + first.ID,
			Priority: 99,
			Seq:      first.Seq,
		}
		seenDep := map[string]bool{}
		for _, m := range members {
			t.Changes = append(t.Changes, m.ID)
			if m.Analysis != nil {
				if p := analyzer.Priority(m.Analysis.Classification); p < t.Priority {
					t.Priority = p
				}
			}
			for _, d := range deps[m.ID] {
				if !in[d] && !seenDep[d] {
					seenDep[d] = true
					t.DependsOn = append(t.DependsOn, d)
				}
			}
			m.TaskID = t.ID
			m.DependsOn = deps[m.ID]
			m.SetStatus(state.StatusPlanned, "")
			res.Changes = append(res.Changes, m)
		}
		if len(members) > 1 {
			t.AddEvent("planned", "grouped dependent changes: "+strings.Join(t.Changes, ", "))
		} else {
			t.AddEvent("planned", "")
		}
		res.Tasks = append(res.Tasks, t)
	}
	return res
}

func dependencies(c *state.Change, history []*state.Change, bySymbol bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && id != c.ID && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	bySHA := func(prefix string) *state.Change {
		if len(prefix) < 7 {
			return nil
		}
		for _, h := range history {
			if strings.HasPrefix(h.SHA, prefix) {
				return h
			}
		}
		return nil
	}
	if c.Analysis != nil {
		for _, d := range c.Analysis.DependsOn {
			if h := bySHA(d); h != nil && h.Seq < c.Seq && active(h.Status) {
				add(h.ID)
			}
		}
	}
	if c.RevertOf != "" {
		if h := bySHA(c.RevertOf); h != nil && active(h.Status) {
			add(h.ID)
		}
	}
	if bySymbol {
		symbolOverlap(c, history, add)
		return out
	}
	paths := map[string]bool{}
	for _, p := range c.Paths() {
		paths[p] = true
	}
	// Walk backwards; take the nearest earlier active change per path.
	remaining := len(paths)
	for i := len(history) - 1; i >= 0 && remaining > 0; i-- {
		h := history[i]
		if h.Seq >= c.Seq {
			continue
		}
		hit := false
		for _, p := range h.Paths() {
			if paths[p] {
				delete(paths, p)
				remaining--
				hit = true
			}
		}
		if hit && active(h.Status) {
			add(h.ID)
		}
	}
	return out
}

// symKey identifies a touched region: a definition in a file, or the whole
// file (sym == "*") when the change is outside definitions or unparsed.
type symKey struct{ path, sym string }

func keysOf(c *state.Change) []symKey {
	var out []symKey
	for _, f := range c.Files {
		if f.OldPath != "" {
			out = append(out, symKey{f.OldPath, "*"})
		}
		if !f.Parsed || f.TopLevel || len(f.Symbols) == 0 {
			out = append(out, symKey{f.Path, "*"})
			continue
		}
		for _, s := range f.Symbols {
			out = append(out, symKey{f.Path, s})
		}
	}
	return out
}

func (k symKey) conflicts(o symKey) bool {
	return k.path == o.path && (k.sym == o.sym || k.sym == "*" || o.sym == "*")
}

// symbolOverlap orders c after the nearest earlier active change touching
// the same definition. A whole-file key keeps looking further back (it
// overlaps every definition), while a definition key is resolved by the
// nearest change touching that definition or the whole file, which is in
// turn ordered after everything before it.
func symbolOverlap(c *state.Change, history []*state.Change, add func(string)) {
	remaining := keysOf(c)
	for i := len(history) - 1; i >= 0 && len(remaining) > 0; i-- {
		h := history[i]
		if h.Seq >= c.Seq {
			continue
		}
		hk := keysOf(h)
		hit := false
		kept := remaining[:0:0]
		for _, k := range remaining {
			resolved := false
			for _, x := range hk {
				if k.conflicts(x) {
					hit = true
					if k.sym != "*" || x.sym == "*" {
						resolved = true
					}
					break
				}
			}
			if !resolved {
				kept = append(kept, k)
			}
		}
		remaining = kept
		if hit && active(h.Status) {
			add(h.ID)
		}
	}
}

// Readiness describes whether a task can start.
type Readiness struct {
	Ready     bool     `json:"ready"`
	Blocked   bool     `json:"blocked"`
	Reason    string   `json:"reason,omitempty"`
	StackOn   string   `json:"stack_on,omitempty"` // task ID whose branch to build on
	WaitingOn []string `json:"waiting_on,omitempty"`
}

// Check evaluates a task's dependencies.
func Check(t *state.Task, changes map[string]*state.Change, tasks map[string]*state.Task, cfg *config.Config) Readiness {
	var r Readiness
	var stack []string
	for _, d := range t.DependsOn {
		dc, ok := changes[d]
		if !ok || dc.Status.Settled() {
			continue
		}
		if dt, ok := tasks[dc.TaskID]; ok && dt.ID == t.ID {
			continue
		}
		switch {
		case dc.Status.NeedsAttention():
			r.Blocked = true
			r.WaitingOn = append(r.WaitingOn, d)
		case cfg.Planner.Stack && (dc.Status == state.StatusVerified || dc.Status == state.StatusPROpened):
			if dt, ok := tasks[dc.TaskID]; ok && dt.Commit != "" {
				if !contains(stack, dt.ID) {
					stack = append(stack, dt.ID)
				}
				continue
			}
			r.WaitingOn = append(r.WaitingOn, d)
		default:
			r.WaitingOn = append(r.WaitingOn, d)
		}
	}
	switch {
	case r.Blocked:
		r.Reason = "dependency needs attention: " + strings.Join(r.WaitingOn, ", ")
	case len(r.WaitingOn) > 0:
		r.Reason = "waiting for: " + strings.Join(r.WaitingOn, ", ")
	case len(stack) > 1:
		r.WaitingOn = stack
		r.Reason = "waiting for one of several unmerged dependencies to merge: " + strings.Join(stack, ", ")
	default:
		r.Ready = true
		if len(stack) == 1 {
			r.StackOn = stack[0]
		}
	}
	return r
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
