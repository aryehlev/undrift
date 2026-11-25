package analyzer

import (
	"testing"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/mapping"
	"github.com/aryehlev/undrift/internal/state"
)

func change(title string, files ...string) *state.Change {
	c := &state.Change{Title: title, Message: title}
	for _, f := range files {
		c.Files = append(c.Files, state.FileChange{Path: f, Status: "M"})
	}
	return c
}

func TestHeuristicClassification(t *testing.T) {
	cases := []struct {
		title string
		files []string
		want  state.Classification
	}{
		{"fix: prevent XSS in template rendering", []string{"render/html.go"}, state.ClassSecurityFix},
		{"Bump golang.org/x/net to fix CVE-2023-44487", []string{"go.mod", "go.sum"}, state.ClassSecurityFix},
		{"fix: off-by-one in pagination", []string{"api/page.go"}, state.ClassBugFix},
		{"feat(api)!: remove legacy endpoint", []string{"api/legacy.go"}, state.ClassBreakingChange},
		{"feat: add retries", []string{"client/retry.go"}, state.ClassFeature},
		{"perf: avoid allocation in hot loop", []string{"core/loop.go"}, state.ClassPerformance},
		{"Update README", []string{"README.md", "docs/intro.md"}, state.ClassDocumentation},
		{"add table tests", []string{"core/loop_test.go"}, state.ClassTest},
		{"chore(deps): bump lodash from 4.17.20 to 4.17.21", []string{"package.json", "package-lock.json"}, state.ClassDependencyUpdate},
		{"ci: speed up workflow", []string{".github/workflows/ci.yml"}, state.ClassLanguageSpecific},
		{"refactor: split module", []string{"core/a.go", "core/b.go"}, state.ClassRefactoring},
		{"wip", []string{"core/a.go"}, state.ClassNeedsReview},
	}
	for _, c := range cases {
		a := Heuristic(change(c.title, c.files...), mapping.Resolution{})
		if a.Classification != c.want {
			t.Errorf("%q: got %s (%.2f) want %s\n  %v", c.title, a.Classification, a.Confidence, c.want, a.Evidence)
		}
		if a.Reasoning == "" {
			t.Errorf("%q: missing reasoning", c.title)
		}
	}
}

func TestDecideNeverSilentlySkipsLowConfidence(t *testing.T) {
	cfg := config.Default()
	a := &state.Analysis{Classification: state.ClassRefactoring, Confidence: 0.4}
	if s, _ := Decide(a, cfg); s != state.StatusNeedsReview {
		t.Fatalf("low-confidence refactor: %s", s)
	}
	a.Confidence = 0.9
	if s, _ := Decide(a, cfg); s != state.StatusSkipped {
		t.Fatalf("confident refactor: %s", s)
	}
	a = &state.Analysis{Classification: state.ClassSecurityFix, Confidence: 0.3}
	if s, _ := Decide(a, cfg); s != state.StatusAnalyzed {
		t.Fatalf("security fixes always proceed: %s", s)
	}
	a = &state.Analysis{Classification: state.ClassBugFix, Confidence: 0.5, AlreadyAddressed: true}
	if s, _ := Decide(a, cfg); s != state.StatusNeedsReview {
		t.Fatalf("uncertain already-implemented: %s", s)
	}
}

func TestAgentAnalysisValidation(t *testing.T) {
	aa, err := ParseAgentAnalysis([]byte(`{"classification":"bug_fix","confidence":85,"reasoning":"fixes nil deref"}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := aa.ToAnalysis("claude")
	if err != nil || a.Confidence != 0.85 || a.Analyzer != "agent:claude" {
		t.Fatalf("%+v %v", a, err)
	}
	aa.Classification = "vibes"
	if _, err := aa.ToAnalysis("x"); err == nil {
		t.Fatal("expected invalid classification error")
	}
}
