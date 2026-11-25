// Package analyzer classifies upstream changes and decides how the ledger
// should treat them.
//
// Two analyzers exist. The heuristic analyzer is deterministic and free; it
// uses commit conventions, keywords, labels and the shape of the touched
// files. The agent analyzer asks a coding agent for a semantic analysis with
// reasoning and evidence (see prompt.go). Every classification carries
// reasoning, evidence and a confidence; low-confidence decisions that would
// cause a change to be skipped are routed to human review instead.
package analyzer

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/glob"
	"github.com/aryehlev/undrift/internal/mapping"
	"github.com/aryehlev/undrift/internal/memory"
	"github.com/aryehlev/undrift/internal/state"
)

type signal struct {
	class  state.Classification
	score  float64
	reason string
}

var (
	conventionalRe = regexp.MustCompile(`^(\w+)(\([^)]*\))?(!)?:\s`)
	cveRe          = regexp.MustCompile(`(?i)\b(CVE-\d{4}-\d{4,}|GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4})\b`)
	securityRe     = regexp.MustCompile(`(?i)\b(security|vulnerab\w*|exploit\w*|xss|csrf|ssrf|rce|sql injection|injection|path traversal|directory traversal|buffer overflow|out-of-bounds|use-after-free|denial of service|dos attack|sanitiz\w*|escape\w* untrusted|auth bypass|privilege escalation|timing attack)\b`)
	bugRe          = regexp.MustCompile(`(?i)\b(fix(es|ed)?|bug|crash\w*|panic\w*|race|deadlock|leak\w*|incorrect\w*|wrong|regression|broken|nil pointer|null pointer|off-by-one|hang\w*|error handling)\b`)
	featureRe      = regexp.MustCompile(`(?i)^(add|adds|added|support|implement|introduce|new|allow|enable)\b`)
	perfRe         = regexp.MustCompile(`(?i)\b(perf|performance|optimi[sz]\w*|faster|speed\s?up|reduce allocations?|allocation|throughput|latency|cache)\b`)
	refactorRe     = regexp.MustCompile(`(?i)^(refactor|rename|move|cleanup|clean up|simplify|restructure|reorganize|tidy)\b`)
	depsRe         = regexp.MustCompile(`(?i)\b(bump|upgrade|update)\b.*\b(dependency|dependencies|deps|from \S+ to \S+|version)\b|^(chore|build)\(deps\)`)
	breakingRe     = regexp.MustCompile(`(?i)BREAKING[ -]CHANGE`)
)

var docPatterns = []string{"*.md", "*.rst", "*.adoc", "*.txt", "docs/**", "doc/**", "LICENSE*", "AUTHORS*", "CHANGELOG*", "CONTRIBUTORS*", "*.png", "*.svg", "*.jpg"}
var testPatterns = []string{"*_test.*", "*.test.*", "*_spec.*", "*.spec.*", "test_*.py", "test/**", "tests/**", "spec/**", "testdata/**", "**/testdata/**", "**/__tests__/**", "**/test/**", "**/tests/**"}
var depPatterns = []string{"go.mod", "go.sum", "package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "Cargo.toml", "Cargo.lock", "requirements*.txt", "poetry.lock", "Pipfile*", "pyproject.toml", "Gemfile*", "pom.xml", "build.gradle*", "*.csproj", "composer.json", "composer.lock", "mix.exs", "mix.lock"}
var toolingPatterns = []string{".github/**", ".gitlab-ci.yml", ".circleci/**", "Makefile", "Dockerfile*", ".golangci.yml", ".eslintrc*", ".prettierrc*", "tox.ini", ".editorconfig", ".gitignore", ".gitattributes", "renovate.json", ".dependabot/**", ".github/dependabot.yml"}

// IsTestFile reports whether a path looks like a test file.
func IsTestFile(p string) bool { return glob.Any(testPatterns, p) }

func allMatch(files []string, patterns []string) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if !glob.Any(patterns, f) {
			return false
		}
	}
	return true
}

// Heuristic analyzes a change without an agent.
func Heuristic(c *state.Change, res mapping.Resolution) *state.Analysis {
	files := make([]string, 0, len(c.Files))
	for _, f := range c.Files {
		files = append(files, f.Path)
	}
	subject := strings.SplitN(c.Title, "\n", 2)[0]
	text := c.Message + "\n" + c.PRBody
	var sigs []signal
	add := func(cl state.Classification, score float64, reason string) {
		sigs = append(sigs, signal{cl, score, reason})
	}

	// Labels from the upstream PR are strong signals.
	for _, l := range c.Labels {
		ll := strings.ToLower(l)
		switch {
		case strings.Contains(ll, "security"):
			add(state.ClassSecurityFix, 0.9, fmt.Sprintf("upstream label %q", l))
		case strings.Contains(ll, "bug"):
			add(state.ClassBugFix, 0.8, fmt.Sprintf("upstream label %q", l))
		case strings.Contains(ll, "breaking"):
			add(state.ClassBreakingChange, 0.85, fmt.Sprintf("upstream label %q", l))
		case strings.Contains(ll, "feature") || strings.Contains(ll, "enhancement"):
			add(state.ClassFeature, 0.75, fmt.Sprintf("upstream label %q", l))
		case strings.Contains(ll, "perf"):
			add(state.ClassPerformance, 0.75, fmt.Sprintf("upstream label %q", l))
		case strings.Contains(ll, "dependencies") || strings.Contains(ll, "deps"):
			add(state.ClassDependencyUpdate, 0.8, fmt.Sprintf("upstream label %q", l))
		case ll == "documentation" || ll == "docs":
			add(state.ClassDocumentation, 0.8, fmt.Sprintf("upstream label %q", l))
		}
	}

	if m := cveRe.FindString(text); m != "" {
		add(state.ClassSecurityFix, 0.95, "references advisory "+m)
	} else if m := securityRe.FindString(text); m != "" {
		add(state.ClassSecurityFix, 0.75, fmt.Sprintf("security keyword %q", m))
	}
	if breakingRe.MatchString(text) {
		add(state.ClassBreakingChange, 0.85, "message declares BREAKING CHANGE")
	}

	if m := conventionalRe.FindStringSubmatch(subject); m != nil {
		typ := strings.ToLower(m[1])
		scope := strings.ToLower(strings.Trim(m[2], "()"))
		if m[3] == "!" {
			add(state.ClassBreakingChange, 0.85, "conventional commit marked breaking (!)")
		}
		switch {
		case typ == "fix" && scope == "security":
			add(state.ClassSecurityFix, 0.9, "conventional commit fix(security)")
		case typ == "fix":
			add(state.ClassBugFix, 0.8, "conventional commit type fix")
		case typ == "feat":
			add(state.ClassFeature, 0.8, "conventional commit type feat")
		case typ == "perf":
			add(state.ClassPerformance, 0.8, "conventional commit type perf")
		case typ == "refactor" || typ == "style":
			add(state.ClassRefactoring, 0.75, "conventional commit type "+typ)
		case typ == "docs":
			add(state.ClassDocumentation, 0.8, "conventional commit type docs")
		case typ == "test" || typ == "tests":
			add(state.ClassTest, 0.8, "conventional commit type test")
		case scope == "deps" || typ == "deps":
			add(state.ClassDependencyUpdate, 0.85, "conventional commit scope deps")
		case typ == "ci" || typ == "build" || typ == "chore":
			add(state.ClassLanguageSpecific, 0.55, "conventional commit type "+typ+" (often tooling-specific)")
		}
	}

	switch {
	case allMatch(files, docPatterns):
		add(state.ClassDocumentation, 0.9, "only documentation files changed")
	case allMatch(files, testPatterns):
		add(state.ClassTest, 0.85, "only test files changed")
	case allMatch(files, depPatterns):
		add(state.ClassDependencyUpdate, 0.85, "only dependency manifests changed")
	case allMatch(files, toolingPatterns):
		add(state.ClassLanguageSpecific, 0.7, "only upstream build/CI tooling changed")
	case allMatch(files, append(append([]string{}, depPatterns...), toolingPatterns...)):
		add(state.ClassLanguageSpecific, 0.65, "only manifests and tooling changed")
	}

	if m := depsRe.FindString(subject); m != "" {
		add(state.ClassDependencyUpdate, 0.7, fmt.Sprintf("dependency keyword %q", m))
	}
	if m := bugRe.FindString(subject); m != "" {
		add(state.ClassBugFix, 0.65, fmt.Sprintf("bug-fix keyword %q in subject", m))
	}
	if m := perfRe.FindString(subject); m != "" {
		add(state.ClassPerformance, 0.6, fmt.Sprintf("performance keyword %q in subject", m))
	}
	if m := refactorRe.FindString(subject); m != "" {
		add(state.ClassRefactoring, 0.6, fmt.Sprintf("refactoring keyword %q in subject", m))
	}
	if m := featureRe.FindString(subject); m != "" {
		add(state.ClassFeature, 0.55, fmt.Sprintf("feature keyword %q in subject", m))
	}

	// A security fix is a specialized bug fix: when both kinds of signal are
	// present, the bug-fix evidence corroborates the security classification
	// instead of outranking it.
	var secBest, bugBest float64
	for _, s := range sigs {
		switch s.class {
		case state.ClassSecurityFix:
			secBest = max(secBest, s.score)
		case state.ClassBugFix:
			bugBest = max(bugBest, s.score)
		}
	}
	if secBest > 0 && bugBest > 0 {
		add(state.ClassSecurityFix, min(0.95, max(secBest, bugBest)+0.05), "security keyword in a bug-fix change")
	}

	a := &state.Analysis{Analyzer: "heuristic", AnalyzedAt: state.Now()}
	if len(sigs) == 0 {
		a.Classification = state.ClassNeedsReview
		a.Confidence = 0.3
		a.Reasoning = "No conventional-commit type, label, keyword or file-shape signal identified the nature of this change."
	} else {
		sort.SliceStable(sigs, func(i, j int) bool { return sigs[i].score > sigs[j].score })
		best := sigs[0]
		a.Classification = best.class
		a.Confidence = best.score
		// Corroborating signals increase confidence; contradicting ones lower it.
		for _, s := range sigs[1:] {
			if s.class == best.class {
				a.Confidence += (1 - a.Confidence) * 0.3
			} else if s.score >= best.score-0.1 && !compatible(best.class, s.class) {
				a.Confidence -= 0.1
			}
		}
		a.Confidence = clamp(a.Confidence)
		var parts []string
		for _, s := range sigs {
			a.Evidence = append(a.Evidence, fmt.Sprintf("%s: %s (%.2f)", s.class, s.reason, s.score))
			if s.class == best.class {
				parts = append(parts, s.reason)
			}
		}
		a.Reasoning = fmt.Sprintf("Classified as %s because: %s.", best.class, strings.Join(parts, "; "))
	}
	if c.RevertOf != "" {
		a.Evidence = append(a.Evidence, "reverts upstream commit "+c.RevertOf)
	}
	a.AffectedComponents = components(files)
	a.DownstreamComponents = res.Downstream
	a.BehaviorChange = subject
	a.Risk = risk(a.Classification)
	a.SuggestedTests = suggestTests(c)
	if len(res.Unsupported) > 0 && len(res.Unsupported) == len(files) {
		f := false
		a.Applicable = &f
		a.Evidence = append(a.Evidence, "all touched files map to functionality marked unsupported downstream")
	}
	return a
}

func compatible(a, b state.Classification) bool {
	pair := map[state.Classification]state.Classification{
		state.ClassSecurityFix:      state.ClassBugFix,
		state.ClassBugFix:           state.ClassSecurityFix,
		state.ClassDependencyUpdate: state.ClassLanguageSpecific,
		state.ClassLanguageSpecific: state.ClassDependencyUpdate,
		state.ClassBreakingChange:   state.ClassFeature,
		state.ClassFeature:          state.ClassBreakingChange,
	}
	return pair[a] == b
}

func clamp(f float64) float64 {
	if f < 0.05 {
		return 0.05
	}
	if f > 0.99 {
		return 0.99
	}
	return float64(int(f*100+0.5)) / 100
}

func components(files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		d := path.Dir(f)
		if d == "." {
			d = f
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func risk(c state.Classification) string {
	switch c {
	case state.ClassSecurityFix:
		return "high: skipping leaves a known vulnerability downstream"
	case state.ClassBreakingChange:
		return "high: public API or behavior changes incompatibly"
	case state.ClassBugFix:
		return "medium: skipping leaves a known defect downstream"
	case state.ClassFeature, state.ClassPerformance:
		return "medium: behavioral surface grows; verify parity"
	default:
		return "low"
	}
}

func suggestTests(c *state.Change) []string {
	var out []string
	for _, f := range c.Files {
		if glob.Any(testPatterns, f.Path) && f.Status != "D" {
			out = append(out, f.Path)
		}
	}
	return out
}

// Decide maps an analysis to a ledger status according to the sync policy.
// It never silently skips low-confidence changes.
func Decide(a *state.Analysis, cfg *config.Config) (state.Status, string) {
	minConf := cfg.Analysis.MinConfidence
	low := a.Confidence < minConf
	if a.Classification == state.ClassNeedsReview {
		return state.StatusNeedsReview, "analyzer requested human review"
	}
	if a.AlreadyAddressed || a.Classification == state.ClassAlreadyImplemented {
		if low {
			return state.StatusNeedsReview, fmt.Sprintf("possibly already implemented (confidence %.2f < %.2f)", a.Confidence, minConf)
		}
		return state.StatusAlreadyImplemented, "analysis found the behavior already present downstream"
	}
	if (a.Applicable != nil && !*a.Applicable) || a.Classification == state.ClassNotApplicable {
		if low {
			return state.StatusNeedsReview, fmt.Sprintf("possibly not applicable (confidence %.2f < %.2f)", a.Confidence, minConf)
		}
		return state.StatusNotApplicable, "analysis found the change does not apply downstream"
	}
	enabled, policy := policyFor(a.Classification, cfg.Sync)
	if !enabled {
		if low {
			return state.StatusNeedsReview, fmt.Sprintf("would be skipped by policy %q but confidence %.2f < %.2f", policy, a.Confidence, minConf)
		}
		return state.StatusSkipped, fmt.Sprintf("sync policy: %s disabled", policy)
	}
	return state.StatusAnalyzed, ""
}

func policyFor(c state.Classification, p config.SyncPolicy) (bool, string) {
	switch c {
	case state.ClassSecurityFix:
		return p.SecurityFixes, "security_fixes"
	case state.ClassBugFix:
		return p.BugFixes, "bug_fixes"
	case state.ClassFeature:
		return p.Features, "features"
	case state.ClassPerformance:
		return p.Performance, "performance"
	case state.ClassBreakingChange:
		return p.BreakingChanges, "breaking_changes"
	case state.ClassRefactoring:
		return p.Refactoring, "refactoring"
	case state.ClassDependencyUpdate:
		return p.Dependencies, "dependencies"
	case state.ClassTest:
		return p.Tests, "tests"
	case state.ClassDocumentation:
		return p.Documentation, "documentation"
	case state.ClassLanguageSpecific:
		return false, "language_specific"
	}
	return true, string(c)
}

// Priority orders work: lower runs first.
func Priority(c state.Classification) int {
	switch c {
	case state.ClassSecurityFix:
		return 0
	case state.ClassBugFix:
		return 1
	case state.ClassBreakingChange:
		return 2
	case state.ClassFeature:
		return 3
	case state.ClassPerformance:
		return 4
	case state.ClassDependencyUpdate:
		return 5
	case state.ClassTest:
		return 6
	case state.ClassRefactoring:
		return 7
	case state.ClassDocumentation:
		return 8
	}
	return 5
}

// DeviationBlocks reports whether a deviation forbids syncing behavior for
// a change (sync_behavior: false on every touched file).
func DeviationBlocks(c *state.Change, devs []memory.Deviation) (bool, string) {
	if len(devs) == 0 {
		return false, ""
	}
	for _, f := range c.Paths() {
		covered := false
		for _, d := range devs {
			if !d.SyncBehavior && glob.Any(stripSymbols(d.Upstream), f) {
				covered = true
				break
			}
		}
		if !covered {
			return false, ""
		}
	}
	return true, "all touched files are covered by deviations with sync_behavior: false"
}

func stripSymbols(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		if j := strings.Index(s, "#"); j >= 0 {
			s = s[:j]
		}
		out[i] = s
	}
	return out
}
