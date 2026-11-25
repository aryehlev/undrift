// Package pullrequest generates downstream pull requests for verified
// synchronization tasks and talks to the hosting provider.
package pullrequest

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/aryehlev/undrift/internal/memory"
	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/upstream"
)

// Marker associates a PR with its task; it is used for duplicate detection.
func Marker(taskID string, changes []string) string {
	return fmt.Sprintf("<!-- undrift:task=%s changes=%s -->", taskID, strings.Join(changes, ","))
}

var conventionalRe = regexp.MustCompile(`^\w+(\([^)]*\))?!?:\s+`)

var markerRe = regexp.MustCompile(`<!-- undrift:task=([^ ]+) changes=([^ ]*) -->`)

// ParseMarker extracts the task ID from a PR body.
func ParseMarker(body string) (task string, changes []string, ok bool) {
	m := markerRe.FindStringSubmatch(body)
	if m == nil {
		return "", nil, false
	}
	return m[1], strings.Split(m[2], ","), true
}

// Title builds a PR title such as
// "[Undrift] Port auction timeout fix from upstream #1842".
func Title(prefix string, changes []*state.Change) string {
	if len(changes) == 0 {
		return prefix + " Synchronize upstream changes"
	}
	c := changes[0]
	title := conventionalRe.ReplaceAllString(strings.TrimSpace(c.Title), "")
	if len(title) > 72 {
		title = title[:69] + "..."
	}
	ref := c.Ref()
	if c.PRNumber == 0 {
		ref = c.Short()
	}
	s := fmt.Sprintf("%s Port %s from upstream %s", prefix, lowerFirst(title), ref)
	if len(changes) > 1 {
		s += fmt.Sprintf(" (+%d related)", len(changes)-1)
	}
	return strings.TrimSpace(s)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	// Keep acronyms and identifiers ("CVE-…", "HTTP") intact.
	if len(s) > 1 && strings.ToUpper(s[:2]) == s[:2] {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// BodyInput holds everything that goes into a PR description.
type BodyInput struct {
	Task         *state.Task
	Changes      []*state.Change
	Result       *state.AgentResult
	Report       *state.VerificationReport
	Deviations   []memory.Deviation
	Dependencies []*state.Change
	Agent        string
	Draft        bool
}

// Body renders a PR description.
func Body(in BodyInput) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("## Upstream change\n\n")
	for _, c := range in.Changes {
		link := c.Short()
		if u := upstream.CommitURL(c.UpstreamRepo, c.SHA); u != "" {
			link = fmt.Sprintf("[%s](%s)", c.Short(), u)
		}
		w("- **%s** — %s", sanitize(c.Title), link)
		if c.PRURL != "" {
			w(" · [upstream PR #%d](%s)", c.PRNumber, c.PRURL)
		} else if c.PRNumber > 0 {
			w(" · upstream PR #%d", c.PRNumber)
		}
		if c.Analysis != nil {
			w(" · `%s`", c.Analysis.Classification)
		}
		w("\n")
	}
	w("\n### What changed upstream\n\n")
	for _, c := range in.Changes {
		if c.Analysis != nil && c.Analysis.Reasoning != "" {
			if c.Analysis.BehaviorChange != "" {
				w("%s\n\n", sanitize(c.Analysis.BehaviorChange))
			}
			w("> %s\n\n", sanitize(c.Analysis.Reasoning))
		}
	}

	w("## Downstream implementation\n\n")
	if in.Result != nil {
		w("%s\n\n", in.Result.Summary)
		if in.Result.ArchitecturalNotes != "" {
			w("### Architectural differences\n\n%s\n\n", in.Result.ArchitecturalNotes)
		}
	}
	if len(in.Deviations) > 0 {
		w("### Preserved deviations\n\n")
		for _, d := range in.Deviations {
			w("- `%s` → `%s`: %s", strings.Join(d.Upstream, ", "), strings.Join(d.Downstream, ", "), strings.TrimSpace(d.Reason))
			if len(d.Preserve) > 0 {
				w(" (preserved: %s)", strings.Join(d.Preserve, "; "))
			}
			w("\n")
		}
		w("\n")
	}
	if in.Result != nil && len(in.Result.TestsAdded) > 0 {
		w("### Tests added or modified\n\n")
		for _, t := range in.Result.TestsAdded {
			w("- `%s`\n", t)
		}
		w("\n")
	}

	w("## Verification\n\n")
	if in.Report == nil {
		w("Not verified.\n\n")
	} else {
		w("**Outcome: %s** — %s\n\n", strings.ReplaceAll(in.Report.Outcome, "_", " "), in.Report.Summary)
		w("| Check | Level | Result |\n|---|---|---|\n")
		for _, c := range in.Report.Checks {
			icon := map[string]string{"passed": "✅", "failed": "❌", "skipped": "⏭️", "error": "⚠️"}[c.Status]
			name := c.Name
			if c.Command != "" && c.Command != c.Name {
				name = fmt.Sprintf("`%s`", c.Command)
			} else {
				name = fmt.Sprintf("`%s`", name)
			}
			note := c.Status
			if c.Note != "" {
				note += " — " + c.Note
			}
			w("| %s | %s | %s %s |\n", name, c.Level, icon, note)
		}
		if r := in.Report.Regression; r != nil {
			w("\nRegression demonstration: failed before = %v, passes after = %v", r.FailedBefore, r.PassedAfter)
			if r.Note != "" {
				w(" (%s)", r.Note)
			}
			w("\n")
		}
		if len(in.Report.Metrics) > 0 {
			w("\n| Metric | Base | Current | Change |\n|---|---|---|---|\n")
			for _, m := range in.Report.Metrics {
				flag := ""
				if m.Regression {
					flag = " ⚠️"
				}
				w("| %s | %g | %g | %+.2f%%%s |\n", m.Name, m.Base, m.Current, m.ChangePercent, flag)
			}
		}
		w("\n_%s_\n\n", in.Report.Disclaimer)
	}

	if in.Result != nil && len(in.Result.Limitations) > 0 {
		w("## Known limitations\n\n")
		for _, l := range in.Result.Limitations {
			w("- %s\n", l)
		}
		w("\n")
	}
	if len(in.Dependencies) > 0 {
		w("## Related upstream dependencies\n\n")
		for _, d := range in.Dependencies {
			w("- %s %s — %s\n", d.ID, sanitize(d.Title), d.Status)
		}
		w("\n")
	}
	conf := "unknown"
	if in.Result != nil && in.Result.Confidence > 0 {
		conf = fmt.Sprintf("%.0f%%", in.Result.Confidence*100)
	}
	w("---\n\n**Confidence:** %s · **Agent:** %s · **Task:** `%s`", conf, nonEmpty(in.Agent, "external"), in.Task.ID)
	if in.Task.CostUSD > 0 {
		w(" · **Cost:** $%.2f", in.Task.CostUSD)
	}
	if in.Draft {
		w("\n\nOpened as a **draft** because verification is incomplete. Review carefully before merging.")
	}
	w("\n\n_Generated by [Undrift](https://github.com/aryehlev/undrift). Synchronization is complete only once this PR is merged._\n\n")
	w("%s\n", Marker(in.Task.ID, in.Task.Changes))
	return b.String()
}

// sanitize neutralizes upstream text that could ping users or inject markup.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "@", "@​")
	s = strings.ReplaceAll(s, "<!--", "&lt;!--")
	return strings.TrimSpace(strings.SplitN(s, "\n\n", 2)[0])
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
