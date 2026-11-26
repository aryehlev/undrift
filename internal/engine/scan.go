package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/aryehlev/undrift/internal/glob"
	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/upstream"
)

// ScanOptions configures a scan.
type ScanOptions struct {
	Upstream      string // restrict to one upstream
	AcceptRewrite bool
}

// UpstreamScan is the scan result for one upstream.
type UpstreamScan struct {
	Upstream   string   `json:"upstream"`
	Head       string   `json:"head"`
	From       string   `json:"from"`
	Discovered []string `json:"discovered"`
	Ignored    []string `json:"ignored,omitempty"`
	Releases   []string `json:"releases,omitempty"`
	Reverted   []string `json:"reverted,omitempty"`
	Rewrite    *Rewrite `json:"rewrite,omitempty"`
}

// Rewrite describes a detected upstream history rewrite.
type Rewrite struct {
	OldHead    string   `json:"old_head"`
	NewHead    string   `json:"new_head"`
	MergeBase  string   `json:"merge_base"`
	Dropped    []string `json:"dropped"`              // records no longer reachable
	Superseded []string `json:"superseded,omitempty"` // matched to rewritten commits
	NeedReview []string `json:"need_review,omitempty"`
	Handled    bool     `json:"handled"`
}

// ScanResult aggregates scan results.
type ScanResult struct {
	Upstreams []UpstreamScan `json:"upstreams"`
	Total     int            `json:"total_discovered"`
}

// Scan discovers new upstream changes and records them in the ledger.
// Scanning is idempotent: known changes are never duplicated.
func (e *Engine) Scan(ctx context.Context, o ScanOptions) (*ScanResult, error) {
	res := &ScanResult{}
	for _, u := range e.Cfg.Upstreams {
		if o.Upstream != "" && u.Name != o.Upstream {
			continue
		}
		w, err := e.Watcher(u.Name)
		if err != nil {
			return nil, err
		}
		e.logf("fetching upstream %s (%s %s)", u.Name, u.Repository, u.Branch)
		if err := w.Fetch(ctx); err != nil {
			return nil, err
		}
		us, err := e.scanUpstream(ctx, w, o)
		if us != nil {
			res.Upstreams = append(res.Upstreams, *us)
			res.Total += len(us.Discovered)
		}
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func (e *Engine) scanUpstream(ctx context.Context, w *upstream.Watcher, o ScanOptions) (*UpstreamScan, error) {
	u := w.Cfg
	head, err := w.Head(ctx)
	if err != nil {
		return nil, err
	}
	meta, err := e.Store.Meta()
	if err != nil {
		return nil, err
	}
	cur := meta.Cursors[u.Name]
	if cur == nil {
		cur = &state.Cursor{}
		meta.Cursors[u.Name] = cur
	}
	from := cur.LastScanned
	if from == "" && u.Baseline != "" {
		if from, err = w.Resolve(ctx, u.Baseline); err != nil {
			return nil, err
		}
	}
	us := &UpstreamScan{Upstream: u.Name, Head: head, From: from}
	if from == "" {
		// No baseline: start tracking from the current head.
		e.logf("upstream %s has no baseline; tracking changes after %s", u.Name, short(head))
		cur.LastScanned, cur.Head, cur.ScannedAt = head, head, state.Now()
		return us, e.Store.Lock(func() error { return e.Store.PutMeta(meta) })
	}

	var rw *Rewrite
	if from != head && !w.Contains(ctx, from, head) {
		mb, _ := w.Mirror.MergeBase(ctx, from, head)
		rw = &Rewrite{OldHead: from, NewHead: head, MergeBase: mb}
		us.Rewrite = rw
		if !o.AcceptRewrite && u.OnRewrite != "reconcile" {
			// Report what would be affected without changing anything.
			cs, _ := e.Store.Changes()
			for _, c := range cs {
				if c.Upstream == u.Name && !w.Contains(ctx, c.SHA, head) {
					rw.Dropped = append(rw.Dropped, c.ID)
				}
			}
			return us, fmt.Errorf("%w: %s %s is no longer an ancestor of %s (%d recorded changes unreachable); rerun with --accept-rewrite to reconcile", ErrHistoryRewrite, u.Name, short(from), short(head), len(rw.Dropped))
		}
		if mb == "" {
			return us, fmt.Errorf("%w: no common history between %s and %s", ErrHistoryRewrite, short(from), short(head))
		}
		from = mb
	}

	changes, err := w.List(ctx, from, head)
	if err != nil {
		return us, err
	}
	ignoreRes := compileAll(u.IgnoreMessages)

	err = e.Store.Lock(func() error {
		all, err := e.Store.Changes()
		if err != nil {
			return err
		}
		bySHA := map[string]*state.Change{}
		for _, c := range all {
			if c.Upstream == u.Name {
				bySHA[c.SHA] = c
			}
		}
		newByPatch := map[string]*state.Change{}
		for _, c := range changes {
			if _, known := bySHA[c.SHA]; known || e.Store.HasChange(c.ID) {
				continue
			}
			cur.Seq++
			c.Seq = cur.Seq
			c.AddEvent("discovered", "scanned from "+u.Repository)
			e.enrich(ctx, w, c)
			if reason := ignored(c, u.IgnoreAuthors, ignoreRes, u.Include, u.Exclude); reason != "" {
				c.SetStatus(state.StatusNotApplicable, reason)
				us.Ignored = append(us.Ignored, c.ID)
			}
			if len(c.Tags) > 0 {
				us.Releases = append(us.Releases, strings.Join(c.Tags, ","))
			}
			if c.PatchID != "" {
				newByPatch[c.PatchID] = c
			}
			bySHA[c.SHA] = c
			us.Discovered = append(us.Discovered, c.ID)
		}

		if rw != nil {
			e.reconcileRewrite(ctx, w, rw, all, newByPatch, head)
		}

		// Reverts: a revert of a change that was never synchronized cancels
		// both; a revert of synchronized work becomes a normal task.
		for _, id := range us.Discovered {
			c := bySHA[shaOf(changes, id)]
			if c == nil || c.RevertOf == "" || c.Status != state.StatusDiscovered {
				continue
			}
			var target *state.Change
			for _, o := range bySHA {
				if strings.HasPrefix(o.SHA, c.RevertOf) {
					target = o
				}
			}
			if target == nil {
				continue
			}
			c.RevertOf = target.SHA
			if preImplementation(target.Status) {
				target.SetStatus(state.StatusReverted, "reverted upstream by "+c.Short())
				c.SetStatus(state.StatusReverted, "reverts "+target.Short()+", which was never synchronized")
				us.Reverted = append(us.Reverted, target.ID, c.ID)
				if err := e.Store.PutChange(target); err != nil {
					return err
				}
			}
		}
		for _, c := range changes {
			if contains(us.Discovered, c.ID) {
				if err := e.Store.PutChange(c); err != nil {
					return err
				}
			}
		}
		cur.LastScanned, cur.Head, cur.ScannedAt = head, head, state.Now()
		return e.Store.PutMeta(meta)
	})
	if err != nil {
		return us, err
	}
	if len(us.Discovered) > 0 {
		e.logf("upstream %s: discovered %d change(s)", u.Name, len(us.Discovered))
	}
	return us, nil
}

// reconcileRewrite marks records that disappeared from upstream history.
func (e *Engine) reconcileRewrite(ctx context.Context, w *upstream.Watcher, rw *Rewrite, all []*state.Change, newByPatch map[string]*state.Change, head string) {
	for _, old := range all {
		if old.Upstream != w.Cfg.Name || w.Contains(ctx, old.SHA, head) {
			continue
		}
		rw.Dropped = append(rw.Dropped, old.ID)
		repl := newByPatch[old.PatchID]
		if old.PatchID == "" {
			repl = nil
		}
		switch {
		case repl != nil:
			old.SupersededBy = repl.ID
			wasSynced := old.Status == state.StatusMerged || old.Status == state.StatusAlreadyImplemented ||
				old.Status == state.StatusPROpened || old.Status == state.StatusVerified
			if wasSynced {
				repl.SetStatus(state.StatusAlreadyImplemented, "equivalent to "+old.ID+" (same patch), synchronized before upstream history was rewritten")
			}
			if !old.Status.Settled() || wasSynced {
				old.SetStatus(state.StatusSuperseded, "upstream history rewritten; replaced by "+repl.ID)
			}
			rw.Superseded = append(rw.Superseded, old.ID)
		case old.Status == state.StatusMerged || old.Status == state.StatusPROpened || old.Status == state.StatusVerified:
			old.SetStatus(state.StatusNeedsReview, "upstream history rewritten: this change was synchronized but no longer exists upstream; decide whether to revert it downstream")
			rw.NeedReview = append(rw.NeedReview, old.ID)
		case !old.Status.Settled():
			old.SetStatus(state.StatusSuperseded, "upstream history rewritten: change no longer exists upstream")
			rw.Superseded = append(rw.Superseded, old.ID)
		}
		old.AddEvent("upstream_rewrite", fmt.Sprintf("%s → %s", short(rw.OldHead), short(rw.NewHead)))
		_ = e.Store.PutChange(old)
	}
	rw.Handled = true
}

func (e *Engine) enrich(ctx context.Context, w *upstream.Watcher, c *state.Change) {
	if c.PRNumber == 0 || w.Cfg.HostAPI == "never" || !upstream.SupportsPRMeta(w.Cfg.Repository) {
		return
	}
	if w.Cfg.HostAPI == "auto" && !upstream.HasToken(w.Cfg.Repository) {
		return
	}
	pr, err := upstream.FetchPR(ctx, w.Cfg.Repository, c.PRNumber)
	if err != nil {
		c.AddEvent("enrich_failed", err.Error())
		return
	}
	if pr.Title != "" {
		c.Title = pr.Title
	}
	c.PRBody = truncate(pr.Body, 20_000)
	c.Labels = pr.Labels
	if pr.URL != "" {
		c.PRURL = pr.URL
	}
}

func ignored(c *state.Change, authors []string, msgs []*regexp.Regexp, include, exclude []string) string {
	for _, a := range authors {
		if strings.EqualFold(a, c.Author) || strings.EqualFold(a, c.AuthorEmail) {
			return "ignored author " + a
		}
	}
	for _, re := range msgs {
		if re.MatchString(c.Message) {
			return "message matches upstream.ignore_messages " + re.String()
		}
	}
	if len(include) == 0 && len(exclude) == 0 {
		return ""
	}
	relevant := 0
	for _, p := range c.Paths() {
		if len(include) > 0 && !glob.Any(include, p) {
			continue
		}
		if glob.Any(exclude, p) {
			continue
		}
		relevant++
	}
	if relevant == 0 && len(c.Files) > 0 {
		return "all touched files are outside upstream.include or inside upstream.exclude"
	}
	return ""
}

func preImplementation(s state.Status) bool {
	switch s {
	case state.StatusDiscovered, state.StatusPending, state.StatusAnalyzed, state.StatusPlanned,
		state.StatusWaitingDependency, state.StatusNeedsReview, state.StatusSkipped,
		state.StatusNotApplicable, state.StatusBlocked, state.StatusFailed:
		return true
	}
	return false
}

func compileAll(pats []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range pats {
		if re, err := regexp.Compile(p); err == nil {
			out = append(out, re)
		}
	}
	return out
}

func shaOf(cs []*state.Change, id string) string {
	for _, c := range cs {
		if c.ID == id {
			return c.SHA
		}
	}
	return ""
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}
