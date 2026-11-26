//go:build cgo

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/aryehlev/undrift/internal/memory"
)

func TestSymbolAwareMappingAndContext(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	commit(t, f.downstream, "rust port", map[string]string{
		"src/auction/engine.rs": "pub fn execute() {}\n\npub fn score_bid(p: u64) -> u64 { p }\n",
	})
	run(t, f.downstream, "git", "push", "-q", "origin", "main")
	if err := f.e.UpdateMemory(func(m *memory.Memory) error {
		m.UpsertMapping(memory.Mapping{Kind: memory.KindFunction, Source: "human",
			Upstream: memory.StringList{"auction/auction.go#RunAuction"}, Downstream: memory.StringList{"src/auction/engine.rs#execute"},
			Relationship: "Execute an auction"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	commit(t, f.upstream, "add scoring", map[string]string{"auction/auction.go": "package auction\n\nfunc RunAuction() {}\n\nfunc scoreBid(p int) int {\n\treturn p\n}\n"})
	f.prepare(ctx)

	run1 := commit(t, f.upstream, "fix: deadline in RunAuction", map[string]string{"auction/auction.go": "package auction\n\nfunc RunAuction() {\n\t// deadline\n}\n\nfunc scoreBid(p int) int {\n\treturn p\n}\n"})
	score := commit(t, f.upstream, "fix: negative scores", map[string]string{"auction/auction.go": "package auction\n\nfunc RunAuction() {\n\t// deadline\n}\n\nfunc scoreBid(p int) int {\n\tif p < 0 {\n\t\treturn 0\n\t}\n\treturn p\n}\n"})
	f.prepare(ctx)

	c1 := f.change(run1)
	if refs := c1.SymbolRefs(); len(refs) != 1 || refs[0] != "auction/auction.go#RunAuction" {
		t.Fatalf("run1 symbols %v (files %+v)", refs, c1.Files)
	}
	t1, _ := f.e.Store.GetTask(c1.TaskID)
	ctr, err := f.e.BuildContract(ctx, t1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctr.Mappings) != 1 || len(ctr.Downstream.Files) != 1 || ctr.Downstream.Files[0].Path != "src/auction/engine.rs" {
		t.Fatalf("run1 mappings %+v files %+v", ctr.Mappings, ctr.Downstream.Files)
	}
	if len(ctr.Changes[0].Symbols) != 1 {
		t.Fatalf("contract symbols %v", ctr.Changes[0].Symbols)
	}

	// The RunAuction mapping does not apply to a change touching only
	// scoreBid; a function-level suggestion is found across conventions.
	c2 := f.change(score)
	t2, _ := f.e.Store.GetTask(c2.TaskID)
	ctr2, err := f.e.BuildContract(ctx, t2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctr2.Mappings) != 0 {
		t.Fatalf("symbol-specific mapping should not apply: %+v", ctr2.Mappings)
	}
	if len(ctr2.SuggestedMappings) != 1 || ctr2.SuggestedMappings[0].Downstream[0] != "src/auction/engine.rs#score_bid" {
		t.Fatalf("suggestions %+v", ctr2.SuggestedMappings)
	}
	prompt := renderPrompt(ctr2)
	for _, want := range []string{"Definitions touched upstream", "auction/auction.go#scoreBid", "Relevant downstream code", "fn score_bid"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
