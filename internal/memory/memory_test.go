package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSpecDeviationAndUpsert(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "deviations.yaml"), []byte(`deviations:
  - upstream: auction/auction.go
    downstream: src/auction/engine.rs
    reason: >
      The Rust implementation uses FuturesUnordered.
    preserve:
      - Single-task execution
    sync_behavior: true
    sync_implementation: false
`), 0o644)
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Deviations) != 1 || m.Deviations[0].Upstream[0] != "auction/auction.go" {
		t.Fatalf("%+v", m.Deviations)
	}
	if len(m.DeviationsFor([]string{"auction/auction.go"}, nil)) != 1 {
		t.Fatal("deviation lookup")
	}
	human := Mapping{Upstream: StringList{"a.go"}, Downstream: StringList{"a.rs"}, Source: "human", Relationship: "human words"}
	m.UpsertMapping(human)
	if m.UpsertMapping(Mapping{Upstream: StringList{"a.go"}, Downstream: StringList{"a.rs"}, Source: "agent", Relationship: "agent words"}) {
		t.Fatal("agent overwrote a human mapping")
	}
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	m2, _ := Load(dir)
	if len(m2.Mappings) != 1 || m2.Mappings[0].Relationship != "human words" || len(m2.MappingsFor([]string{"a.go"})) != 1 {
		t.Fatalf("%+v", m2.Mappings)
	}
}
