package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aryehlev/undrift/internal/git"
	"github.com/aryehlev/undrift/internal/state"
)

// ErrLeaseHeld is returned when another worker holds a task's lease.
var ErrLeaseHeld = errors.New("task is leased by another worker")

// LeaseBackend provides mutual exclusion for tasks. The local backend relies
// on the state store lock and is sufficient for workers on one machine. The
// git backend stores leases as refs on the shared downstream remote and uses
// compare-and-swap pushes (--force-with-lease), which makes it safe for
// workers on different machines that share only the git remote.
type LeaseBackend interface {
	Name() string
	Acquire(ctx context.Context, taskID string, l *state.Lease, prev *state.Lease) error
	Renew(ctx context.Context, taskID string, l *state.Lease) error
	Release(ctx context.Context, taskID string, l *state.Lease) error
}

// NewToken returns a random lease token.
func NewToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LocalLeases is a no-op backend: exclusivity comes from the store lock.
type LocalLeases struct{}

// Name implements LeaseBackend.
func (LocalLeases) Name() string { return "local" }

// Acquire implements LeaseBackend.
func (LocalLeases) Acquire(context.Context, string, *state.Lease, *state.Lease) error { return nil }

// Renew implements LeaseBackend.
func (LocalLeases) Renew(context.Context, string, *state.Lease) error { return nil }

// Release implements LeaseBackend.
func (LocalLeases) Release(context.Context, string, *state.Lease) error { return nil }

// GitLeases stores leases as refs/undrift/leases/<task> on a remote.
type GitLeases struct {
	Repo   *git.Repo
	Remote string
}

// Name implements LeaseBackend.
func (g *GitLeases) Name() string { return "git" }

func leaseRef(taskID string) string { return "refs/undrift/leases/" + taskID }

type remoteLease struct {
	Owner     string    `json:"owner"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// remoteState returns the remote lease commit SHA and its decoded payload.
func (g *GitLeases) remoteState(ctx context.Context, ref string) (string, *remoteLease, error) {
	out, err := g.Repo.Run(ctx, "ls-remote", g.Remote, ref)
	if err != nil {
		return "", nil, err
	}
	f := strings.Fields(out)
	if len(f) < 1 {
		return "", nil, nil
	}
	sha := f[0]
	if _, err := g.Repo.Run(ctx, "fetch", "--quiet", g.Remote, "+"+ref+":"+ref); err != nil {
		return sha, nil, err
	}
	msg, err := g.Repo.Run(ctx, "log", "-1", "--format=%B", sha)
	if err != nil {
		return sha, nil, err
	}
	var rl remoteLease
	if err := json.Unmarshal([]byte(strings.TrimSpace(msg)), &rl); err != nil {
		return sha, nil, nil // unreadable lease: treat as expired
	}
	return sha, &rl, nil
}

func (g *GitLeases) leaseCommit(ctx context.Context, l *state.Lease) (string, error) {
	tree, err := g.Repo.RunEnv(ctx, nil, []byte{}, "mktree")
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(remoteLease{Owner: l.Owner, Token: l.Token, ExpiresAt: l.ExpiresAt})
	return g.Repo.RunEnv(ctx, []string{
		"GIT_AUTHOR_NAME=undrift", "GIT_AUTHOR_EMAIL=undrift@localhost",
		"GIT_COMMITTER_NAME=undrift", "GIT_COMMITTER_EMAIL=undrift@localhost",
	}, nil, "commit-tree", tree, "-m", string(payload))
}

// cas pushes newSHA to ref only if the remote ref currently equals expect
// ("" means the ref must not exist). newSHA "" deletes the ref.
func (g *GitLeases) cas(ctx context.Context, ref, expect, newSHA string) error {
	_, err := g.Repo.Run(ctx, "push", "--quiet", "--force-with-lease="+ref+":"+expect, g.Remote, newSHA+":"+ref)
	if err != nil {
		if strings.Contains(err.Error(), "stale info") || strings.Contains(err.Error(), "rejected") || strings.Contains(err.Error(), "already exists") {
			return ErrLeaseHeld
		}
		return err
	}
	return nil
}

// Acquire implements LeaseBackend.
func (g *GitLeases) Acquire(ctx context.Context, taskID string, l *state.Lease, _ *state.Lease) error {
	ref := leaseRef(taskID)
	cur, rl, err := g.remoteState(ctx, ref)
	if err != nil {
		return err
	}
	if rl != nil && time.Now().Before(rl.ExpiresAt) && rl.Token != l.Token {
		return fmt.Errorf("%w (owner %s until %s)", ErrLeaseHeld, rl.Owner, rl.ExpiresAt.Format(time.RFC3339))
	}
	commit, err := g.leaseCommit(ctx, l)
	if err != nil {
		return err
	}
	if err := g.cas(ctx, ref, cur, commit); err != nil {
		return err
	}
	l.Ref, l.RefSHA = ref, commit
	return nil
}

// Renew implements LeaseBackend.
func (g *GitLeases) Renew(ctx context.Context, taskID string, l *state.Lease) error {
	ref := leaseRef(taskID)
	cur, rl, err := g.remoteState(ctx, ref)
	if err != nil {
		return err
	}
	if rl == nil || rl.Token != l.Token {
		return fmt.Errorf("%w: lease lost", ErrLeaseHeld)
	}
	commit, err := g.leaseCommit(ctx, l)
	if err != nil {
		return err
	}
	if err := g.cas(ctx, ref, cur, commit); err != nil {
		return err
	}
	l.RefSHA = commit
	return nil
}

// Release implements LeaseBackend.
func (g *GitLeases) Release(ctx context.Context, taskID string, l *state.Lease) error {
	ref := leaseRef(taskID)
	cur, rl, err := g.remoteState(ctx, ref)
	if err != nil || cur == "" {
		return err
	}
	if rl != nil && rl.Token != l.Token {
		return nil // someone else owns it now; nothing to release
	}
	return g.cas(ctx, ref, cur, "")
}
