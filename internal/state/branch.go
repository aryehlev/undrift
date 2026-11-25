package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aryehlev/undrift/internal/git"
)

// BranchSync persists the ledger directory on a dedicated orphan branch of
// the downstream remote. It allows ephemeral environments (CI runners) to
// share durable state without a database. Records are merged per file:
// the copy with the higher revision (then later update time) wins, which is
// safe because every record is only ever mutated under a task lease or the
// store lock.
type BranchSync struct {
	Repo   *git.Repo
	Remote string
	Branch string
	Dir    string
}

const stateTrackingRef = "refs/undrift/state-remote"

// Pull fetches the state branch and merges it into Dir. It returns the
// number of records updated locally.
func (b *BranchSync) Pull(ctx context.Context) (int, error) {
	if _, err := b.Repo.Run(ctx, "fetch", "--quiet", b.Remote, "+refs/heads/"+b.Branch+":"+stateTrackingRef); err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") || strings.Contains(err.Error(), "not our ref") {
			return 0, nil // branch does not exist yet
		}
		return 0, err
	}
	out, err := b.Repo.Run(ctx, "ls-tree", "-r", "--name-only", stateTrackingRef)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range git.SplitLines(out) {
		remote, err := b.Repo.RunRaw(ctx, nil, "cat-file", "blob", stateTrackingRef+":"+p)
		if err != nil {
			return n, err
		}
		local := filepath.Join(b.Dir, filepath.FromSlash(p))
		cur, err := os.ReadFile(local)
		if err == nil && !newer(remote, cur) {
			continue
		}
		if err := WriteFileAtomic(local, remote); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Push merges remote state and publishes Dir to the state branch, retrying
// on concurrent updates.
func (b *BranchSync) Push(ctx context.Context, message string) error {
	gitDir, err := b.Repo.Run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := b.Pull(ctx); err != nil {
			return err
		}
		parent := ""
		if sha, err := b.Repo.RevParse(ctx, stateTrackingRef); err == nil {
			parent = sha
		}
		index := filepath.Join(os.TempDir(), fmt.Sprintf("undrift-state-index-%d-%d", os.Getpid(), time.Now().UnixNano()))
		env := []string{"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + b.Dir, "GIT_INDEX_FILE=" + index}
		wt := &git.Repo{Dir: b.Dir}
		if _, err := wt.RunEnv(ctx, env, nil, "add", "-A", "-f", "--", "."); err != nil {
			os.Remove(index)
			return err
		}
		tree, err := wt.RunEnv(ctx, env, nil, "write-tree")
		os.Remove(index)
		if err != nil {
			return err
		}
		if parent != "" {
			if pt, err := b.Repo.Run(ctx, "rev-parse", parent+"^{tree}"); err == nil && pt == tree {
				return nil // nothing changed
			}
		}
		args := []string{"commit-tree", tree, "-m", message}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		commit, err := b.Repo.RunEnv(ctx, []string{
			"GIT_AUTHOR_NAME=undrift", "GIT_AUTHOR_EMAIL=undrift@localhost",
			"GIT_COMMITTER_NAME=undrift", "GIT_COMMITTER_EMAIL=undrift@localhost",
		}, nil, args...)
		if err != nil {
			return err
		}
		if _, err := b.Repo.Run(ctx, "push", "--quiet", b.Remote, commit+":refs/heads/"+b.Branch); err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		_, _ = b.Repo.Run(ctx, "update-ref", stateTrackingRef, commit)
		return nil
	}
	return fmt.Errorf("push state branch: %w", lastErr)
}

type versioned struct {
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

// newer reports whether record a should replace record b.
func newer(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return false
	}
	var va, vb versioned
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	if va.Revision != vb.Revision {
		return va.Revision > vb.Revision
	}
	return va.UpdatedAt.After(vb.UpdatedAt)
}
