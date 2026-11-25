// Package upstream watches upstream repositories. It maintains a local bare
// mirror, enumerates logical changes (first-parent history, so merged pull
// requests and squash merges are one change each), and extracts metadata
// such as PR numbers, reverts, tags and patch IDs.
package upstream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/git"
	"github.com/aryehlev/undrift/internal/state"
)

// Watcher tracks one upstream repository.
type Watcher struct {
	Cfg    config.Upstream
	Mirror *git.Repo
}

// New returns a watcher whose mirror lives under runDir.
func New(cfg config.Upstream, runDir string) *Watcher {
	return &Watcher{Cfg: cfg, Mirror: git.OpenBare(filepath.Join(runDir, "upstream", cfg.Name+".git"))}
}

// TrackingRef is the mirror ref holding the upstream branch head.
func (w *Watcher) TrackingRef() string { return "refs/remotes/upstream/" + w.Cfg.Branch }

// Fetch creates or updates the mirror.
func (w *Watcher) Fetch(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(w.Mirror.Dir, "HEAD")); err != nil {
		if err := os.MkdirAll(w.Mirror.Dir, 0o755); err != nil {
			return err
		}
		if _, err := git.Open(w.Mirror.Dir).Run(ctx, "init", "--bare", "--quiet"); err != nil {
			return err
		}
	}
	_, err := w.Mirror.Run(ctx, "fetch", "--quiet", "--force", "--tags", "--prune",
		repoURL(w.Cfg.Repository), "+refs/heads/"+w.Cfg.Branch+":"+w.TrackingRef())
	if err != nil {
		return fmt.Errorf("fetch upstream %s: %w", w.Cfg.Name, err)
	}
	return nil
}

// repoURL converts local relative paths to absolute so that the mirror can
// fetch from them regardless of working directory.
func repoURL(r string) string {
	if strings.Contains(r, "://") || strings.HasPrefix(r, "git@") {
		return r
	}
	if abs, err := filepath.Abs(r); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	return r
}

// Head returns the current upstream head SHA.
func (w *Watcher) Head(ctx context.Context) (string, error) {
	return w.Mirror.RevParse(ctx, w.TrackingRef())
}

// Resolve resolves a revision (SHA, tag) in the mirror.
func (w *Watcher) Resolve(ctx context.Context, rev string) (string, error) {
	sha, err := w.Mirror.RevParse(ctx, rev)
	if err != nil {
		return "", fmt.Errorf("upstream %s: cannot resolve %q", w.Cfg.Name, rev)
	}
	return sha, nil
}

// ChangeID returns the stable ledger identity of an upstream commit.
func (w *Watcher) ChangeID(sha string) string {
	id := sha
	if len(id) > 12 {
		id = id[:12]
	}
	if w.Cfg.Name != "upstream" {
		id = w.Cfg.Name + "-" + id
	}
	return id
}

const (
	fieldSep  = "\x1f"
	recordSep = "\x1e"
)

// List returns logical changes in (from, to], oldest first. If from is
// empty, history from the root is listed.
func (w *Watcher) List(ctx context.Context, from, to string) ([]*state.Change, error) {
	rng := to
	if from != "" {
		rng = from + ".." + to
	}
	out, err := w.Mirror.Run(ctx, "log", "--first-parent", "--reverse",
		"--format=%H"+fieldSep+"%P"+fieldSep+"%an"+fieldSep+"%ae"+fieldSep+"%aI"+fieldSep+"%cI"+fieldSep+"%B"+recordSep, rng)
	if err != nil {
		return nil, err
	}
	tags, _ := w.tagsBySHA(ctx)
	patchIDs, _ := w.patchIDs(ctx, rng)
	var changes []*state.Change
	for _, rec := range strings.Split(out, recordSep) {
		rec = strings.TrimLeft(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		f := strings.SplitN(rec, fieldSep, 7)
		if len(f) < 7 {
			continue
		}
		c := &state.Change{
			ID:           w.ChangeID(f[0]),
			Upstream:     w.Cfg.Name,
			UpstreamRepo: w.Cfg.Repository,
			SHA:          f[0],
			Parents:      strings.Fields(f[1]),
			Author:       f[2],
			AuthorEmail:  f[3],
			Message:      strings.TrimSpace(f[6]),
			Tags:         tags[f[0]],
			PatchID:      patchIDs[f[0]],
			Status:       state.StatusDiscovered,
		}
		c.AuthoredAt, _ = time.Parse(time.RFC3339, f[4])
		c.CommittedAt, _ = time.Parse(time.RFC3339, f[5])
		c.IsMerge = len(c.Parents) > 1
		c.Title, c.PRNumber = ParseTitle(c.Message, c.IsMerge)
		c.RevertOf = ParseRevert(c.Message)
		if c.PRNumber > 0 {
			c.PRURL = PRURL(w.Cfg.Repository, c.PRNumber)
		}
		files, err := w.Files(ctx, c)
		if err != nil {
			return nil, err
		}
		c.Files = files
		changes = append(changes, c)
	}
	return changes, nil
}

// Files lists files changed relative to the first parent.
func (w *Watcher) Files(ctx context.Context, c *state.Change) ([]state.FileChange, error) {
	args := []string{"diff-tree", "--no-commit-id", "-r", "--name-status", "-M"}
	if len(c.Parents) == 0 {
		args = append(args, "--root", c.SHA)
	} else {
		args = append(args, c.Parents[0], c.SHA)
	}
	out, err := w.Mirror.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var files []state.FileChange
	for _, l := range git.SplitLines(out) {
		parts := strings.Split(l, "\t")
		if len(parts) < 2 {
			continue
		}
		fc := state.FileChange{Status: parts[0][:1], Path: parts[len(parts)-1]}
		if len(parts) == 3 {
			fc.OldPath = parts[1]
		}
		files = append(files, fc)
	}
	return files, nil
}

// Diff returns the patch of a change relative to its first parent,
// truncated to maxBytes (0 = unlimited).
func (w *Watcher) Diff(ctx context.Context, c *state.Change, maxBytes int) (string, bool, error) {
	var args []string
	if len(c.Parents) == 0 {
		args = []string{"show", "--format=", "--patch", "--stat", c.SHA}
	} else {
		args = []string{"diff", "--stat", "--patch", "-M", c.Parents[0], c.SHA}
	}
	out, err := w.Mirror.Run(ctx, args...)
	if err != nil {
		return "", false, err
	}
	if maxBytes > 0 && len(out) > maxBytes {
		return out[:maxBytes] + "\n... [diff truncated]\n", true, nil
	}
	return out, false, nil
}

// FileAt returns file contents at a revision.
func (w *Watcher) FileAt(ctx context.Context, rev, path string) ([]byte, error) {
	return w.Mirror.ShowFile(ctx, rev, path)
}

// Contains reports whether sha is reachable from the upstream head.
func (w *Watcher) Contains(ctx context.Context, sha, head string) bool {
	ok, err := w.Mirror.IsAncestor(ctx, sha, head)
	return err == nil && ok
}

func (w *Watcher) tagsBySHA(ctx context.Context) (map[string][]string, error) {
	out, err := w.Mirror.Run(ctx, "for-each-ref", "refs/tags", "--format=%(objectname) %(*objectname) %(refname:short)")
	if err != nil {
		return nil, err
	}
	m := map[string][]string{}
	for _, l := range git.SplitLines(out) {
		f := strings.Fields(l)
		switch len(f) {
		case 2:
			m[f[0]] = append(m[f[0]], f[1])
		case 3:
			m[f[1]] = append(m[f[1]], f[2])
		}
	}
	return m, nil
}

// patchIDs computes stable patch IDs (relative to first parent) so that a
// rewritten upstream history can be matched against previously synced work.
func (w *Watcher) patchIDs(ctx context.Context, rng string) (map[string]string, error) {
	logOut, err := w.Mirror.RunRaw(ctx, nil, "log", "--first-parent", "--diff-merges=first-parent", "-p", "--format=commit %H", rng)
	if err != nil {
		return nil, err
	}
	out, err := w.Mirror.RunRaw(ctx, logOut, "patch-id", "--stable")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range git.SplitLines(string(out)) {
		f := strings.Fields(l)
		if len(f) == 2 {
			m[f[1]] = f[0]
		}
	}
	return m, nil
}

var (
	mergePRRe  = regexp.MustCompile(`^Merge pull request #(\d+)`)
	squashPRRe = regexp.MustCompile(`\(#(\d+)\)\s*$`)
	glMergeRe  = regexp.MustCompile(`See merge request [^!]*!(\d+)`)
	revertRe   = regexp.MustCompile(`This reverts commit ([0-9a-f]{7,40})`)
)

// ParseTitle extracts a human title and pull request number from a commit
// message.
func ParseTitle(msg string, isMerge bool) (string, int) {
	lines := strings.Split(msg, "\n")
	subject := strings.TrimSpace(lines[0])
	if m := mergePRRe.FindStringSubmatch(subject); m != nil {
		n, _ := strconv.Atoi(m[1])
		for _, l := range lines[1:] {
			if l = strings.TrimSpace(l); l != "" {
				return l, n
			}
		}
		return subject, n
	}
	if m := squashPRRe.FindStringSubmatch(subject); m != nil {
		n, _ := strconv.Atoi(m[1])
		return strings.TrimSpace(squashPRRe.ReplaceAllString(subject, "")), n
	}
	if m := glMergeRe.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return subject, n
	}
	return subject, 0
}

// ParseRevert returns the SHA reverted by a commit message, if any.
func ParseRevert(msg string) string {
	if m := revertRe.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return ""
}

// GitHubSlug extracts "owner/repo" from a GitHub URL, or "".
func GitHubSlug(url string) string {
	u := strings.TrimSuffix(strings.TrimSpace(url), ".git")
	for _, p := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "ssh://git@github.com/"} {
		if strings.HasPrefix(u, p) {
			s := strings.TrimPrefix(u, p)
			if parts := strings.Split(s, "/"); len(parts) >= 2 {
				return parts[0] + "/" + parts[1]
			}
		}
	}
	return ""
}

// PRURL returns a browsable pull request URL for GitHub upstreams.
func PRURL(repo string, n int) string {
	if slug := GitHubSlug(repo); slug != "" {
		return fmt.Sprintf("https://github.com/%s/pull/%d", slug, n)
	}
	return ""
}

// CommitURL returns a browsable commit URL for GitHub upstreams.
func CommitURL(repo, sha string) string {
	if slug := GitHubSlug(repo); slug != "" {
		return fmt.Sprintf("https://github.com/%s/commit/%s", slug, sha)
	}
	return ""
}
