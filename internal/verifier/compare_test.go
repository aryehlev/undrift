package verifier

import (
	"testing"

	"github.com/aryehlev/undrift/internal/config"
)

func TestNormalize(t *testing.T) {
	n := config.Normalize{IgnoreKeys: []string{"id", "ts"}, SortArrays: true}
	a := Normalize(`{"id": 1, "bids": [3, 1, 2], "ts": "2024", "winner": "x"}`, n)
	b := Normalize(`{"winner":"x","bids":[1,2,3],"id":99}`, n)
	if a != b {
		t.Fatalf("\n%s\n!=\n%s", a, b)
	}
	p := config.Normalize{IgnorePattern: []string{`\d{4}-\d{2}-\d{2}`}, SortLines: true}
	if Normalize("b 2024-01-01\na", p) != Normalize("a\nb 1999-12-31", p) {
		t.Fatal("pattern/sort normalization")
	}
}

func TestMetricsCompare(t *testing.T) {
	base, _ := ParseMetrics(`{"metrics": {"latency_ms": 10, "throughput": 1000}}`)
	cur, _ := ParseMetrics("latency_ms 10.4\nthroughput 900\n")
	ms, reg := Compare(base, cur, 5, []string{"throughput"})
	if len(ms) != 2 || len(reg) != 1 || reg[0][:10] != "throughput" {
		t.Fatalf("metrics %+v regressions %v", ms, reg)
	}
}
