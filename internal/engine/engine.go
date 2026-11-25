// Package engine is the Undrift core. It owns synchronization state and the
// workflow (scan → analyze → plan → implement → verify → PR → merged) and
// treats coding agents as interchangeable workers.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/git"
	"github.com/aryehlev/undrift/internal/memory"
	"github.com/aryehlev/undrift/internal/pullrequest"
	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/tasks"
	"github.com/aryehlev/undrift/internal/upstream"
	"github.com/aryehlev/undrift/internal/workspace"
)

// Sentinel errors mapped to documented exit codes by the CLI.
var (
	ErrNothingToDo        = errors.New("nothing to do")
	ErrVerificationFailed = errors.New("verification failed")
	ErrHistoryRewrite     = errors.New("upstream history was rewritten")
	ErrNeedsReview        = errors.New("human review required")
)

// Engine ties the components together.
type Engine struct {
	Cfg   *config.Config
	Store *state.Store
	Repo  *git.Repo
	Tasks *tasks.Manager
	WS    *workspace.Manager
	Out   io.Writer

	mu       sync.Mutex
	watchers map[string]*upstream.Watcher
	provider pullrequest.Provider
	memMu    sync.Mutex
}

// Open creates an engine for a loaded configuration.
func Open(cfg *config.Config, out io.Writer) (*Engine, error) {
	if out == nil {
		out = io.Discard
	}
	st, err := state.Open(filepath.Join(cfg.StateDir(), "state"), filepath.Join(cfg.RunDir(), "locks"))
	if err != nil {
		return nil, err
	}
	repo := git.Open(cfg.Root)
	var leases tasks.LeaseBackend = tasks.LocalLeases{}
	if cfg.State.Lease == "git" {
		leases = &tasks.GitLeases{Repo: repo, Remote: cfg.Downstream.Remote}
	}
	return &Engine{
		Cfg:      cfg,
		Store:    st,
		Repo:     repo,
		Tasks:    &tasks.Manager{Store: st, Cfg: cfg, Leases: leases},
		WS:       workspace.New(repo, cfg.RunDir()),
		Out:      out,
		watchers: map[string]*upstream.Watcher{},
	}, nil
}

func (e *Engine) logf(format string, a ...any) {
	fmt.Fprintf(e.Out, "undrift: "+format+"\n", a...)
}

// Watcher returns the watcher for an upstream.
func (e *Engine) Watcher(name string) (*upstream.Watcher, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if w, ok := e.watchers[name]; ok {
		return w, nil
	}
	u, ok := e.Cfg.UpstreamByName(name)
	if !ok {
		return nil, fmt.Errorf("unknown upstream %q", name)
	}
	w := upstream.New(*u, e.Cfg.RunDir())
	e.watchers[name] = w
	return w, nil
}

// ensureMirror fetches the upstream mirror if it does not exist yet (e.g.
// on a fresh CI runner that restored state from the state branch).
func (e *Engine) ensureMirror(ctx context.Context, name string) (*upstream.Watcher, error) {
	w, err := e.Watcher(name)
	if err != nil {
		return nil, err
	}
	if _, err := w.Head(ctx); err != nil {
		if err := w.Fetch(ctx); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// MemoryDir is the version-controlled memory directory.
func (e *Engine) MemoryDir() string { return filepath.Join(e.Cfg.StateDir(), "memory") }

// Memory loads project memory from the downstream checkout.
func (e *Engine) Memory() (*memory.Memory, error) { return memory.Load(e.MemoryDir()) }

// UpdateMemory loads, mutates and saves memory atomically.
func (e *Engine) UpdateMemory(fn func(*memory.Memory) error) error {
	e.memMu.Lock()
	defer e.memMu.Unlock()
	m, err := e.Memory()
	if err != nil {
		return err
	}
	if err := fn(m); err != nil {
		return err
	}
	return m.Save()
}

// DownstreamSlug returns "owner/name" for the downstream repository.
func (e *Engine) DownstreamSlug(ctx context.Context) string {
	if e.Cfg.Downstream.Repository != "" {
		return e.Cfg.Downstream.Repository
	}
	if s := upstream.GitHubSlug(e.Repo.RemoteURL(ctx, e.Cfg.Downstream.Remote)); s != "" {
		return s
	}
	return os.Getenv("GITHUB_REPOSITORY")
}

// Provider returns the PR provider, or nil if PRs are disabled.
func (e *Engine) Provider(ctx context.Context) (pullrequest.Provider, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.provider != nil {
		return e.provider, nil
	}
	if e.Cfg.PullRequests.Provider == "none" {
		return nil, nil
	}
	p, err := pullrequest.NewGitHub(e.DownstreamSlug(ctx))
	if err != nil {
		return nil, err
	}
	e.provider = p
	return p, nil
}

// SetProvider overrides the PR provider (tests, custom hosts).
func (e *Engine) SetProvider(p pullrequest.Provider) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.provider = p
}

// TaskDir holds per-task runtime files (prompt, task.json, result, logs).
func (e *Engine) TaskDir(taskID string) string {
	return filepath.Join(e.Cfg.RunDir(), "tasks", taskID)
}

// changesOf loads a task's changes.
func (e *Engine) changesOf(t *state.Task) ([]*state.Change, error) {
	out := make([]*state.Change, 0, len(t.Changes))
	for _, id := range t.Changes {
		c, err := e.Store.GetChange(id)
		if err != nil {
			return nil, fmt.Errorf("task %s: change %s: %w", t.ID, id, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// updateChanges applies fn to each change of a task and saves it.
func (e *Engine) updateChanges(t *state.Task, fn func(*state.Change)) error {
	return e.Store.Lock(func() error {
		for _, id := range t.Changes {
			c, err := e.Store.GetChange(id)
			if err != nil {
				return err
			}
			fn(c)
			if err := e.Store.PutChange(c); err != nil {
				return err
			}
		}
		return nil
	})
}

// saveTask saves a task under the store lock. A concurrent lease renewal
// (from the renewer goroutine) is preserved rather than overwritten.
func (e *Engine) saveTask(t *state.Task) error {
	return e.Store.Lock(func() error {
		if cur, err := e.Store.GetTask(t.ID); err == nil {
			if cur.Lease != nil && t.Lease != nil && cur.Lease.Token == t.Lease.Token && cur.Lease.ExpiresAt.After(t.Lease.ExpiresAt) {
				t.Lease.ExpiresAt, t.Lease.RefSHA = cur.Lease.ExpiresAt, cur.Lease.RefSHA
			}
			if cur.Revision > t.Revision {
				t.Revision = cur.Revision
			}
		}
		return e.Store.PutTask(t)
	})
}

// InitOptions configures `undrift init`.
type InitOptions struct {
	Dir           string
	Upstream      string
	Branch        string
	Baseline      string
	Agent         string
	Verify        []string
	Force         bool
	StateOnBranch bool
}

// InitResult reports what init created.
type InitResult struct {
	Config   string   `json:"config"`
	Baseline string   `json:"baseline"`
	Created  []string `json:"created"`
}

// Init creates undrift.yaml and the .undrift directory. If no baseline is
// given, the current upstream head is used: the port is assumed to be in
// sync as of now, and only later upstream changes are tracked.
func Init(ctx context.Context, o InitOptions) (*InitResult, error) {
	if o.Dir == "" {
		o.Dir = "."
	}
	root, err := git.Toplevel(ctx, o.Dir)
	if err != nil {
		return nil, fmt.Errorf("undrift init must run inside the downstream git repository: %w", err)
	}
	if o.Upstream == "" {
		return nil, fmt.Errorf("--upstream is required")
	}
	if o.Branch == "" {
		o.Branch = "main"
	}
	if o.Agent == "" {
		o.Agent = "claude"
	}
	cfgPath := filepath.Join(root, config.FileName)
	if _, err := os.Stat(cfgPath); err == nil && !o.Force {
		return nil, fmt.Errorf("%s already exists (use --force to overwrite)", cfgPath)
	}
	res := &InitResult{Config: cfgPath}

	// Resolve the baseline against the real upstream.
	tmp, err := config.Parse([]byte(config.Template(o.Upstream, o.Branch, o.Baseline, o.Agent, o.Verify)))
	if err != nil {
		return nil, err
	}
	tmp.Root = root
	w := upstream.New(tmp.Upstreams[0], tmp.RunDir())
	if err := w.Fetch(ctx); err != nil {
		return nil, err
	}
	baseline := o.Baseline
	if baseline == "" {
		baseline = "refs/remotes/upstream/" + o.Branch
	}
	sha, err := w.Resolve(ctx, baseline)
	if err != nil {
		return nil, err
	}
	res.Baseline = sha

	if err := os.WriteFile(cfgPath, []byte(config.Template(o.Upstream, o.Branch, sha, o.Agent, o.Verify)), 0o644); err != nil {
		return nil, err
	}
	res.Created = append(res.Created, config.FileName)
	sd := tmp.StateDir()
	ignore := "run/\n"
	if o.StateOnBranch {
		ignore += "state/\n"
	}
	files := map[string]string{
		filepath.Join(sd, ".gitignore"):                ignore,
		filepath.Join(sd, "memory", "mappings.yaml"):   memory.StarterMappings,
		filepath.Join(sd, "memory", "deviations.yaml"): memory.StarterDeviations,
		filepath.Join(sd, "memory", "notes.yaml"):      memory.StarterNotes,
	}
	for p, content := range files {
		if _, err := os.Stat(p); err == nil && !o.Force {
			continue
		}
		if err := state.WriteFileAtomic(p, []byte(content)); err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(root, p)
		res.Created = append(res.Created, rel)
	}
	if err := os.MkdirAll(filepath.Join(sd, "state"), 0o755); err != nil {
		return nil, err
	}
	return res, nil
}

func firstLine(s string) string {
	return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
}
