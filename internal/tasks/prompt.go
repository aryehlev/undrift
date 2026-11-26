package tasks

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const untrustedOpen = "<untrusted-upstream-content>"
const untrustedClose = "</untrusted-upstream-content>"

// Fence wraps upstream-controlled text so agents can distinguish data from
// instructions. Any attempt to close the fence early is neutralized.
func Fence(s string) string {
	s = strings.ReplaceAll(s, "</untrusted-upstream-content", "<\\/untrusted-upstream-content")
	s = strings.ReplaceAll(s, "<untrusted-upstream-content", "<\\untrusted-upstream-content")
	return untrustedOpen + "\n" + s + "\n" + untrustedClose
}

// RenderPrompt renders the contract as Markdown instructions for an agent.
func RenderPrompt(c *Contract) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("# Undrift synchronization task %s\n\n", c.TaskID)
	w("%s\n\n", c.Goal)
	w("## Security notice\n\n")
	for _, s := range c.Security {
		w("- %s\n", s)
	}
	w("\n## Instructions\n\n")
	for i, s := range c.Instructions {
		w("%d. %s\n", i+1, s)
	}

	w("\n## Downstream workspace\n\n")
	w("- Work only inside: `%s`\n", c.Downstream.Workdir)
	w("- Branch: `%s` (based on `%s`", c.Downstream.Branch, c.Downstream.Base)
	if c.Downstream.BaseSHA != "" {
		w(" @ %s", short(c.Downstream.BaseSHA))
	}
	w(")\n")
	if c.Downstream.StackedOn != "" {
		w("- This branch is stacked on unmerged task `%s`; its changes are already present.\n", c.Downstream.StackedOn)
	}
	if c.UpstreamCheckout != "" {
		w("- A read-only checkout of upstream at the change is available at `%s` (do not modify or execute it).\n", c.UpstreamCheckout)
	}

	for i, ch := range c.Changes {
		w("\n## Upstream change %d of %d: %s\n\n", i+1, len(c.Changes), ch.ID)
		w("- Repository: %s\n- Commit: %s\n", ch.Repository, ch.SHA)
		if ch.CommitURL != "" {
			w("- Commit URL: %s\n", ch.CommitURL)
		}
		if ch.PRURL != "" {
			w("- Pull request: %s\n", ch.PRURL)
		}
		w("- Author: %s, %s\n", ch.Author, ch.Date)
		if ch.Classification != "" {
			w("- Classification: **%s**", ch.Classification)
			if ch.Analysis != nil {
				w(" (confidence %.2f, %s)", ch.Analysis.Confidence, ch.Analysis.Analyzer)
			}
			w("\n")
		}
		if ch.RevertOf != "" {
			w("- This change REVERTS upstream commit %s. Revert the corresponding downstream behavior.\n", ch.RevertOf)
		}
		if ch.Analysis != nil {
			w("\n### Analysis\n\n%s\n", ch.Analysis.Reasoning)
			if ch.Analysis.BehaviorChange != "" {
				w("\nBehavior change: %s\n", ch.Analysis.BehaviorChange)
			}
			if len(ch.Analysis.SuggestedTests) > 0 {
				w("\nRelevant upstream tests: %s\n", strings.Join(ch.Analysis.SuggestedTests, ", "))
			}
		}
		w("\n### Files changed upstream\n\n")
		for _, f := range ch.Files {
			if f.OldPath != "" {
				w("- %s %s → %s\n", f.Status, f.OldPath, f.Path)
			} else {
				w("- %s %s\n", f.Status, f.Path)
			}
		}
		if len(ch.Symbols) > 0 {
			w("\n### Definitions touched upstream\n\n")
			for _, sym := range ch.Symbols {
				w("- `%s`\n", sym)
			}
		}
		w("\n### Commit message (untrusted)\n\n%s\n", Fence(ch.Message))
		if ch.PRBody != "" {
			w("\n### Pull request description (untrusted)\n\n%s\n", Fence(ch.PRBody))
		}
		w("\n### Upstream diff (untrusted)\n\n%s\n", Fence("```diff\n"+ch.Diff+"\n```"))
		for _, f := range ch.UpstreamFiles {
			w("\n#### Upstream file after change: %s%s (untrusted)\n\n%s\n", f.Path, fileNote(f), Fence("```\n"+f.Content+"\n```"))
		}
	}

	if len(c.Downstream.Files) > 0 {
		w("\n## Relevant downstream code\n\n")
		w("Resolved from mappings and suggestions; it may be incomplete, so search the workspace as needed.\n")
		for _, f := range c.Downstream.Files {
			w("\n#### %s%s\n\n```\n%s\n```\n", f.Path, fileNote(f), f.Content)
		}
	}

	w("\n## Project memory\n\n")
	if len(c.Mappings) > 0 {
		w("### Semantic mappings (upstream → downstream)\n\n```yaml\n%s```\n\n", toYAML(c.Mappings))
	} else {
		w("No stored mappings cover these upstream files. Locate the downstream functionality yourself and report the mapping in your result.\n\n")
	}
	if len(c.SuggestedMappings) > 0 {
		w("### Heuristic mapping suggestions (unverified)\n\n```yaml\n%s```\n\n", toYAML(c.SuggestedMappings))
	}
	if len(c.UnmappedFiles) > 0 {
		w("Unmapped upstream files: %s\n\n", strings.Join(c.UnmappedFiles, ", "))
	}
	if len(c.Deviations) > 0 {
		w("### Intentional deviations — MUST be preserved\n\n```yaml\n%s```\n\n", toYAML(c.Deviations))
		w("Where `sync_implementation: false`, port only the behavior; keep the downstream implementation strategy and everything listed under `preserve`.\n\n")
	}
	if len(c.Notes) > 0 {
		w("### Project notes\n\n")
		for _, n := range c.Notes {
			w("- [%s] %s\n", n.Kind, n.Text)
		}
		w("\n")
	}
	if len(c.Rules) > 0 {
		w("### Rules\n\n")
		for _, r := range c.Rules {
			w("- %s\n", r)
		}
		w("\n")
	}
	if len(c.PreviousDecisions) > 0 {
		w("### Previous decisions on related changes\n\n")
		for _, d := range c.PreviousDecisions {
			w("- %s %q: %s", d.Change, d.Title, d.Status)
			if d.Reason != "" {
				w(" — %s", d.Reason)
			}
			w("\n")
		}
		w("\n")
	}
	if len(c.PreviousAttempts) > 0 {
		w("### Previous attempts on this task (learn from these)\n\n")
		for _, a := range c.PreviousAttempts {
			w("- Attempt %d (%s): %s", a.Number, a.Agent, a.Outcome)
			if a.Error != "" {
				w(" — %s", a.Error)
			}
			if len(a.FailedChecks) > 0 {
				w("\n  Failed checks:\n")
				for _, f := range a.FailedChecks {
					w("  ```\n  %s\n  ```\n", strings.ReplaceAll(f, "\n", "\n  "))
				}
			}
			w("\n")
		}
		w("\n")
	}

	w("## Required behavior\n\n")
	for _, r := range c.RequiredBehavior {
		w("- %s\n", r)
	}
	w("\n## Acceptance criteria\n\n")
	for _, a := range c.AcceptanceCriteria {
		w("- [ ] %s\n", a)
	}
	w("\n## Verification\n\n%s\n\n", c.Verification.Note)
	if len(c.Verification.Commands) > 0 {
		w("Undrift will run these commands (from trusted downstream configuration):\n\n")
		for _, cmd := range c.Verification.Commands {
			w("- `%s`\n", cmd)
		}
		w("\nRun them yourself before finishing.\n")
	}

	w("\n## Output\n\n")
	w("When finished, write a JSON document to `%s` with this shape:\n\n```json\n%s\n```\n\n", c.Output.ResultFile, c.Output.Schema)
	w("Use `implemented` only if you changed downstream code. Use `already_implemented` or `not_applicable` (with evidence in `summary`) if no change is needed, and `needs_human` if you cannot decide safely. Do not commit, push or open pull requests; Undrift does that after verification.\n")
	return b.String()
}

func toYAML(v any) string {
	out, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(out)
}

func fileNote(f FileContent) string {
	switch {
	case f.Excerpt:
		return " [excerpt: " + strings.Join(f.Symbols, ", ") + "]"
	case f.Truncated:
		return " [truncated]"
	}
	return ""
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
