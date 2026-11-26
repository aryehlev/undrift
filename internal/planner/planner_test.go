package planner

import (
	"testing"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/state"
)

func ch(id string, seq int, status state.Status, files ...string) *state.Change {
	c := &state.Change{ID: id, SHA: id + "0000000000000000", Upstream: "upstream", Seq: seq, Status: status,
		Analysis: &state.Analysis{Classification: state.ClassBugFix}}
	for _, f := range files {
		c.Files = append(c.Files, state.FileChange{Path: f})
	}
	return c
}

func TestPlanDependencies(t *testing.T) {
	cfg := config.Default()
	a := ch("aaaaaaa", 1, state.StatusAnalyzed, "iface.go")
	b := ch("bbbbbbb", 2, state.StatusAnalyzed, "iface.go", "impl.go")
	c := ch("ccccccc", 3, state.StatusAnalyzed, "impl.go")
	d := ch("ddddddd", 4, state.StatusAnalyzed, "other.go")
	merged := ch("eeeeeee", 0, state.StatusMerged, "other.go")
	r := Plan([]*state.Change{merged, a, b, c, d}, cfg)
	if len(r.Tasks) != 4 {
		t.Fatalf("tasks: %d", len(r.Tasks))
	}
	deps := map[string][]string{}
	for _, t := range r.Tasks {
		deps[t.Changes[0]] = t.DependsOn
	}
	if len(deps["aaaaaaa"]) != 0 || len(deps["ddddddd"]) != 0 {
		t.Fatalf("independent changes got deps: %v", deps)
	}
	if len(deps["bbbbbbb"]) != 1 || deps["bbbbbbb"][0] != "aaaaaaa" {
		t.Fatalf("b deps: %v", deps["bbbbbbb"])
	}
	if len(deps["ccccccc"]) != 1 || deps["ccccccc"][0] != "bbbbbbb" {
		t.Fatalf("c deps: %v", deps["ccccccc"])
	}
}

func TestGroupDependent(t *testing.T) {
	cfg := config.Default()
	cfg.Planner.GroupDependent = true
	a := ch("aaaaaaa", 1, state.StatusAnalyzed, "x.go")
	b := ch("bbbbbbb", 2, state.StatusAnalyzed, "x.go")
	c := ch("ccccccc", 3, state.StatusAnalyzed, "y.go")
	r := Plan([]*state.Change{a, b, c}, cfg)
	if len(r.Tasks) != 2 || len(r.Tasks[0].Changes) != 2 {
		t.Fatalf("grouping: %+v", r.Tasks)
	}
}

func TestReadinessBlockedVsWaiting(t *testing.T) {
	cfg := config.Default()
	dep := ch("aaaaaaa", 1, state.StatusNeedsReview, "x.go")
	task := &state.Task{ID: "t-b", DependsOn: []string{"aaaaaaa"}}
	r := Check(task, map[string]*state.Change{"aaaaaaa": dep}, map[string]*state.Task{}, cfg)
	if r.Ready || !r.Blocked {
		t.Fatalf("needs-review dep should block: %+v", r)
	}
	dep.Status = state.StatusInProgress
	if r = Check(task, map[string]*state.Change{"aaaaaaa": dep}, map[string]*state.Task{}, cfg); r.Ready || r.Blocked {
		t.Fatalf("in-progress dep should wait: %+v", r)
	}
	dep.Status, dep.TaskID = state.StatusPROpened, "t-a"
	tasks := map[string]*state.Task{"t-a": {ID: "t-a", Commit: "abc", Branch: "undrift/a"}}
	if r = Check(task, map[string]*state.Change{"aaaaaaa": dep}, tasks, cfg); !r.Ready || r.StackOn != "t-a" {
		t.Fatalf("pr-opened dep should stack: %+v", r)
	}
	dep.Status = state.StatusMerged
	if r = Check(task, map[string]*state.Change{"aaaaaaa": dep}, tasks, cfg); !r.Ready || r.StackOn != "" {
		t.Fatalf("merged dep: %+v", r)
	}
}

func withSymbols(c *state.Change, path string, topLevel bool, syms ...string) *state.Change {
	for i := range c.Files {
		if c.Files[i].Path == path {
			c.Files[i].Parsed, c.Files[i].Symbols, c.Files[i].TopLevel = true, syms, topLevel
		}
	}
	return c
}

func TestSymbolGranularity(t *testing.T) {
	cfg := config.Default()
	cfg.Planner.Granularity = "symbol"
	a := withSymbols(ch("aaaaaaa", 1, state.StatusAnalyzed, "auction.go"), "auction.go", false, "RunAuction")
	b := withSymbols(ch("bbbbbbb", 2, state.StatusAnalyzed, "auction.go"), "auction.go", false, "scoreBid")
	c := withSymbols(ch("ccccccc", 3, state.StatusAnalyzed, "auction.go"), "auction.go", false, "RunAuction")
	d := withSymbols(ch("ddddddd", 4, state.StatusAnalyzed, "auction.go"), "auction.go", true) // imports
	e := withSymbols(ch("eeeeeee", 5, state.StatusAnalyzed, "auction.go"), "auction.go", false, "scoreBid")
	r := Plan([]*state.Change{a, b, c, d, e}, cfg)
	deps := map[string][]string{}
	for _, t := range r.Tasks {
		deps[t.Changes[0]] = t.DependsOn
	}
	want := map[string][]string{
		"aaaaaaa": nil,
		"bbbbbbb": nil,                               // different function: independent
		"ccccccc": {"aaaaaaa"},                       // same function as a
		"ddddddd": {"ccccccc", "bbbbbbb", "aaaaaaa"}, // file-level change waits for all
		"eeeeeee": {"ddddddd"},                       // d already orders after b
	}
	for id, w := range want {
		if len(deps[id]) != len(w) {
			t.Errorf("%s deps %v want %v", id, deps[id], w)
			continue
		}
		for i := range w {
			if deps[id][i] != w[i] {
				t.Errorf("%s deps %v want %v", id, deps[id], w)
			}
		}
	}
	// File granularity orders every change to the file.
	cfg.Planner.Granularity = "file"
	for _, x := range []*state.Change{a, b, c, d, e} {
		x.TaskID, x.Status = "", state.StatusAnalyzed
	}
	r = Plan([]*state.Change{a, b, c, d, e}, cfg)
	if got := r.Tasks[1].DependsOn; len(got) != 1 || got[0] != "aaaaaaa" {
		t.Fatalf("file mode b deps %v", got)
	}
}
