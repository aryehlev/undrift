// Package git wraps the git command line. Undrift deliberately shells out to
// git rather than reimplementing it, so behaviour matches what developers see.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Repo is a git repository (worktree or bare) at Dir.
type Repo struct {
	Dir string
	// Bare is true for bare repositories (uses --git-dir).
	Bare bool
	// Env holds extra environment variables (KEY=VALUE).
	Env []string
}

// Open returns a Repo rooted at dir.
func Open(dir string) *Repo { return &Repo{Dir: dir} }

// OpenBare returns a Repo for a bare repository at dir.
func OpenBare(dir string) *Repo { return &Repo{Dir: dir, Bare: true} }

// Error describes a failed git invocation.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode returns the git exit status for err, or -1.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func (r *Repo) cmd(ctx context.Context, args ...string) *exec.Cmd {
	full := args
	if r.Bare {
		full = append([]string{"--git-dir", r.Dir}, args...)
	}
	c := exec.CommandContext(ctx, "git", full...)
	if !r.Bare {
		c.Dir = r.Dir
	}
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	c.Env = append(c.Env, r.Env...)
	return c
}

// Run executes git and returns trimmed stdout.
func (r *Repo) Run(ctx context.Context, args ...string) (string, error) {
	out, err := r.RunRaw(ctx, nil, args...)
	return strings.TrimRight(string(out), "\n"), err
}

// RunRaw executes git with optional stdin and returns raw stdout.
func (r *Repo) RunRaw(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	c := r.cmd(ctx, args...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	if err := c.Run(); err != nil {
		return stdout.Bytes(), &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// RunEnv executes git with additional environment variables.
func (r *Repo) RunEnv(ctx context.Context, env []string, stdin []byte, args ...string) (string, error) {
	cp := *r
	cp.Env = append(append([]string{}, r.Env...), env...)
	out, err := cp.RunRaw(ctx, stdin, args...)
	return strings.TrimRight(string(out), "\n"), err
}

// RevParse resolves a revision to a full object name.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	return r.Run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
}

// HasRev reports whether rev resolves to a commit.
func (r *Repo) HasRev(ctx context.Context, rev string) bool {
	_, err := r.RevParse(ctx, rev)
	return err == nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.Run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if ExitCode(errors.Unwrap(err)) == 1 {
		return false, nil
	}
	return false, err
}

// MergeBase returns the best common ancestor of a and b.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	return r.Run(ctx, "merge-base", a, b)
}

// IsClean reports whether the worktree has no uncommitted changes.
func (r *Repo) IsClean(ctx context.Context) (bool, error) {
	out, err := r.Run(ctx, "status", "--porcelain")
	return out == "", err
}

// CurrentBranch returns the checked-out branch name.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	return r.Run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
}

// RemoteURL returns the URL of a remote, or "" if missing.
func (r *Repo) RemoteURL(ctx context.Context, remote string) string {
	out, err := r.Run(ctx, "remote", "get-url", remote)
	if err != nil {
		return ""
	}
	return out
}

// ShowFile returns the contents of path at rev.
func (r *Repo) ShowFile(ctx context.Context, rev, path string) ([]byte, error) {
	return r.RunRaw(ctx, nil, "show", rev+":"+path)
}

// Toplevel returns the top-level directory of the worktree containing dir.
func Toplevel(ctx context.Context, dir string) (string, error) {
	return Open(dir).Run(ctx, "rev-parse", "--show-toplevel")
}

// SplitLines splits non-empty lines.
func SplitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
