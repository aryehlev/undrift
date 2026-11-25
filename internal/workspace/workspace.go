// Package workspace isolates each task in its own git worktree so that
// tasks can run in parallel without touching the developer's checkout.
package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aryehlev/undrift/internal/git"
)

// Manager creates and removes worktrees of the downstream repository.
type Manager struct {
	Repo *git.Repo
	Dir  string // parent directory for worktrees
}

// New returns a manager placing worktrees under runDir/worktrees.
func New(repo *git.Repo, runDir string) *Manager {
	return &Manager{Repo: repo, Dir: filepath.Join(runDir, "worktrees")}
}

// Path returns the worktree path for a name.
func (m *Manager) Path(name string) string { return filepath.Join(m.Dir, name) }

// Prepare returns a worktree for branch, creating it from base if needed.
// An existing worktree on the same branch is reused, which makes
// interrupted tasks resumable.
func (m *Manager) Prepare(ctx context.Context, name, branch, base string) (string, error) {
	dir := m.Path(name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		cur, err := git.Open(dir).CurrentBranch(ctx)
		if err == nil && cur == branch {
			return dir, nil
		}
		_ = m.Remove(ctx, dir)
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return "", err
	}
	_, _ = m.Repo.Run(ctx, "worktree", "prune")
	// If the branch already exists (e.g. a previous attempt was pushed),
	// reset it to the requested base: attempts always start fresh from base
	// unless the worktree itself survived.
	if _, err := m.Repo.Run(ctx, "worktree", "add", "--quiet", "-B", branch, dir, base); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	return dir, nil
}

// Detached creates a throwaway worktree at rev (used for baselines).
func (m *Manager) Detached(ctx context.Context, name, rev string) (string, func(), error) {
	return detached(ctx, m.Repo, filepath.Join(m.Dir, name), rev)
}

// UpstreamCheckout creates a read-only detached worktree of an upstream
// mirror at sha.
func UpstreamCheckout(ctx context.Context, mirror *git.Repo, dir, sha string) (string, func(), error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if head, err := git.Open(dir).Run(ctx, "rev-parse", "HEAD"); err == nil && head == sha {
			return dir, func() { removeWorktree(context.Background(), mirror, dir) }, nil
		}
		removeWorktree(ctx, mirror, dir)
	}
	return detached(ctx, mirror, dir, sha)
}

func detached(ctx context.Context, repo *git.Repo, dir, rev string) (string, func(), error) {
	removeWorktree(ctx, repo, dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", nil, err
	}
	_, _ = repo.Run(ctx, "worktree", "prune")
	if _, err := repo.Run(ctx, "worktree", "add", "--quiet", "--detach", dir, rev); err != nil {
		return "", nil, fmt.Errorf("create worktree at %s: %w", rev, err)
	}
	return dir, func() { removeWorktree(context.Background(), repo, dir) }, nil
}

func removeWorktree(ctx context.Context, repo *git.Repo, dir string) {
	if _, err := os.Stat(dir); err != nil {
		return
	}
	if _, err := repo.Run(ctx, "worktree", "remove", "--force", dir); err != nil {
		os.RemoveAll(dir)
		_, _ = repo.Run(ctx, "worktree", "prune")
	}
}

// RemoveFrom deletes a worktree belonging to repo (e.g. an upstream mirror).
func RemoveFrom(ctx context.Context, repo *git.Repo, dir string) { removeWorktree(ctx, repo, dir) }

// Remove deletes a worktree.
func (m *Manager) Remove(ctx context.Context, dir string) error {
	removeWorktree(ctx, m.Repo, dir)
	return nil
}

// identityEnv supplies a commit identity when none is configured (CI).
func identityEnv(ctx context.Context, r *git.Repo) []string {
	if name, _ := r.Run(ctx, "config", "user.name"); name != "" {
		if email, _ := r.Run(ctx, "config", "user.email"); email != "" {
			return nil
		}
	}
	return []string{
		"GIT_AUTHOR_NAME=undrift", "GIT_AUTHOR_EMAIL=undrift@users.noreply.github.com",
		"GIT_COMMITTER_NAME=undrift", "GIT_COMMITTER_EMAIL=undrift@users.noreply.github.com",
	}
}

// CommitAll stages and commits every change in dir. It returns the HEAD
// SHA and whether a new commit was created.
func CommitAll(ctx context.Context, dir, message string) (string, bool, error) {
	r := git.Open(dir)
	if _, err := r.Run(ctx, "add", "-A"); err != nil {
		return "", false, err
	}
	staged, err := r.Run(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		return "", false, err
	}
	committed := false
	if strings.TrimSpace(staged) != "" {
		if _, err := r.RunEnv(ctx, identityEnv(ctx, r), nil, "commit", "--quiet", "--no-verify", "-m", message); err != nil {
			return "", false, err
		}
		committed = true
	}
	head, err := r.Run(ctx, "rev-parse", "HEAD")
	return head, committed, err
}

// ChangedFiles lists files changed between base and HEAD in dir.
func ChangedFiles(ctx context.Context, dir, base string) ([]string, error) {
	out, err := git.Open(dir).Run(ctx, "diff", "--name-only", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	return git.SplitLines(out), nil
}
