// Package report renders human-readable synchronization reports.
package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/aryehlev/undrift/internal/engine"
	"github.com/aryehlev/undrift/internal/state"
)

// Data is the input for a report.
type Data struct {
	Status  *engine.Status
	Changes []engine.ChangeLine
	History []engine.HistoryEntry
	Sync    *engine.SyncResult
	Now     time.Time
}

// Markdown renders a synchronization report.
func Markdown(d Data) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	s := d.Status
	w("# Undrift synchronization report\n\n")
	if !d.Now.IsZero() {
		w("_Generated %s_\n\n", d.Now.UTC().Format(time.RFC1123))
	}
	for _, u := range s.Upstreams {
		w("- **%s**: %s (`%s`)", u.Name, u.Repository, u.Branch)
		if u.LastScanned != "" {
			w(", scanned through `%s` at %s", shortSHA(u.LastScanned), u.ScannedAt.Format("2006-01-02 15:04 MST"))
		}
		w("\n")
	}
	pct := 100.0
	if s.Total > 0 {
		pct = float64(s.Synced) / float64(s.Total) * 100
	}
	w("\n**%d** upstream changes tracked · **%d** settled (%.0f%%) · **%d** remaining", s.Total, s.Synced, pct, s.Remaining)
	if s.CostUSD > 0 {
		w(" · agent cost **$%.2f**", s.CostUSD)
	}
	w("\n\n")

	if d.Sync != nil {
		w("## This run (%s)\n\n", d.Sync.Agent)
		if len(d.Sync.Processed) == 0 {
			w("No tasks were processed.")
			if d.Sync.Stopped != "" {
				w(" Stopped: %s.", d.Sync.Stopped)
			}
			w("\n\n")
		} else {
			w("| Task | Outcome | Details |\n|---|---|---|\n")
			for _, p := range d.Sync.Processed {
				w("| `%s` | %s | %s |\n", p.Task, p.Outcome, cell(p.Message))
			}
			if d.Sync.Stopped != "" {
				w("\nStopped: %s.\n", d.Sync.Stopped)
			}
			w("\n")
		}
		for _, t := range d.Sync.Reconciled {
			w("- `%s`: %s → %s (%s)\n", t.Task, t.From, t.To, t.Reason)
		}
	}

	w("## Status\n\n| Status | Changes |\n|---|---|\n")
	for _, st := range state.AllStatuses {
		if n := s.Changes[st]; n > 0 {
			w("| %s | %d |\n", st, n)
		}
	}
	w("\n")
	section := func(title string, lines []engine.ChangeLine) {
		if len(lines) == 0 {
			return
		}
		w("## %s\n\n| Change | Title | Status | Notes |\n|---|---|---|---|\n", title)
		for _, l := range lines {
			note := l.Reason
			if l.PR != "" {
				note = l.PR
			}
			w("| `%s` %s | %s | %s | %s |\n", l.ID, l.Ref, cell(l.Title), l.Status, cell(note))
		}
		w("\n")
	}
	section("Needs attention", s.Attention)
	section("Open pull requests", s.OpenPRs)
	section("In flight", s.InFlight)
	if len(d.History) > 0 {
		w("## Recent decisions\n\n")
		for _, h := range d.History {
			w("- %s `%s` %s by %s", h.At.Format("2006-01-02"), h.Change, h.Action, h.By)
			if h.Detail != "" {
				w(": %s", cell(h.Detail))
			}
			w("\n")
		}
		w("\n")
	}
	w("_Synchronization is complete only after verification and merge. Passing checks are not proof of complete behavioral parity._\n")
	return b.String()
}

func cell(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
