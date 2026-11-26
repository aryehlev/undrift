// Package config loads and validates undrift.yaml.
//
// Every field has a sensible default so that a minimal configuration only
// needs an upstream repository.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the primary configuration file name.
const FileName = "undrift.yaml"

// StateDirName is the directory (relative to the downstream root) that holds
// Undrift memory, ledger and runtime data.
const StateDirName = ".undrift"

// Config is the root configuration document.
type Config struct {
	Version      int                 `yaml:"version"`
	Upstream     *Upstream           `yaml:"upstream,omitempty"`
	Upstreams    []Upstream          `yaml:"upstreams,omitempty"`
	Downstream   Downstream          `yaml:"downstream"`
	Agent        Agent               `yaml:"agent"`
	Agents       map[string]AgentDef `yaml:"agents,omitempty"`
	Analysis     Analysis            `yaml:"analysis"`
	Sync         SyncPolicy          `yaml:"sync"`
	Planner      Planner             `yaml:"planner"`
	Verification Verification        `yaml:"verification"`
	Rules        []string            `yaml:"rules,omitempty"`
	PullRequests PullRequests        `yaml:"pull_requests"`
	Watch        Watch               `yaml:"watch"`
	Limits       Limits              `yaml:"limits"`
	State        State               `yaml:"state"`
	Security     Security            `yaml:"security"`
	Context      Context             `yaml:"context"`

	// Root is the downstream repository root (directory containing the
	// configuration file). It is not serialized.
	Root string `yaml:"-"`
	// Path is the absolute path of the loaded configuration file.
	Path string `yaml:"-"`
}

// Upstream describes one upstream repository to track.
type Upstream struct {
	Name       string `yaml:"name,omitempty"`
	Repository string `yaml:"repository"`
	Branch     string `yaml:"branch,omitempty"`
	Baseline   string `yaml:"baseline,omitempty"`
	// Include restricts tracking to changes touching these paths (globs).
	Include []string `yaml:"include,omitempty"`
	// Exclude ignores files matching these globs when deciding applicability.
	Exclude []string `yaml:"exclude,omitempty"`
	// IgnoreAuthors lists author names or emails whose changes are recorded
	// as not applicable (e.g. bots).
	IgnoreAuthors []string `yaml:"ignore_authors,omitempty"`
	// IgnoreMessages lists regular expressions matched against commit
	// messages; matching changes are recorded as not applicable.
	IgnoreMessages []string `yaml:"ignore_messages,omitempty"`
	// OnRewrite controls behaviour when upstream history is rewritten:
	// "stop" (default) requires `undrift scan --accept-rewrite`;
	// "reconcile" handles it automatically.
	OnRewrite string `yaml:"on_rewrite,omitempty"`
	// HostAPI enables enrichment of changes with pull/merge request
	// metadata (title, description, labels) from the GitHub or GitLab API:
	// "auto" (when a token is available), "always", "never".
	HostAPI string `yaml:"host_api,omitempty"`
}

// Downstream describes the repository Undrift is keeping in sync.
type Downstream struct {
	Branch string `yaml:"branch"`
	Remote string `yaml:"remote"`
	// Repository overrides the "owner/name" used for pull requests. When
	// empty it is derived from the remote URL.
	Repository string `yaml:"repository,omitempty"`
}

// Agent selects and tunes the coding agent used for managed execution.
type Agent struct {
	Provider         string   `yaml:"provider"`
	Model            string   `yaml:"model,omitempty"`
	MaxAttempts      int      `yaml:"max_attempts"`
	MaxParallelTasks int      `yaml:"max_parallel_tasks"`
	Timeout          Duration `yaml:"timeout"`
	// Env lists additional environment variable names passed to agents.
	Env []string `yaml:"env,omitempty"`
}

// AgentDef defines (or overrides) a command-line agent adapter. Community
// adapters are added here without modifying Undrift.
type AgentDef struct {
	// Command is the executable to run.
	Command string `yaml:"command"`
	// Args for implementation tasks. Supports placeholders: {{prompt}},
	// {{prompt_file}}, {{task_file}}, {{result_file}}, {{workdir}}, {{model}}.
	Args []string `yaml:"args,omitempty"`
	// AnalyzeArgs are used for read-only analysis tasks (defaults to Args).
	AnalyzeArgs []string `yaml:"analyze_args,omitempty"`
	// Prompt delivery: "stdin" (default) or "none" (prompt is referenced
	// through placeholders in Args only).
	Prompt string `yaml:"prompt,omitempty"`
	// Output parser: "text" (default), "claude-json".
	Output string `yaml:"output,omitempty"`
	// Env lists environment variable names forwarded to this agent.
	Env []string `yaml:"env,omitempty"`
}

// Analysis configures the semantic change analyzer.
type Analysis struct {
	// Agent used for semantic analysis. Empty means heuristic only.
	Agent string `yaml:"agent,omitempty"`
	// MinConfidence below which decisions require human review instead of
	// being applied automatically.
	MinConfidence float64 `yaml:"min_confidence"`
}

// SyncPolicy chooses which classes of change are synchronized.
type SyncPolicy struct {
	SecurityFixes        bool `yaml:"security_fixes"`
	BugFixes             bool `yaml:"bug_fixes"`
	Features             bool `yaml:"features"`
	Performance          bool `yaml:"performance"`
	BreakingChanges      bool `yaml:"breaking_changes"`
	Refactoring          bool `yaml:"refactoring"`
	Dependencies         bool `yaml:"dependencies"`
	Tests                bool `yaml:"tests"`
	Documentation        bool `yaml:"documentation"`
	PreserveArchitecture bool `yaml:"preserve_architecture"`
}

// Planner configures dependency planning.
type Planner struct {
	// GroupDependent merges chains of dependent changes into one task.
	GroupDependent bool `yaml:"group_dependent"`
	MaxGroupSize   int  `yaml:"max_group_size"`
	// Stack allows a task to build on an unmerged dependency's branch.
	Stack bool `yaml:"stack"`
	// Granularity of overlap-based dependencies: "file" (two changes to the
	// same file are ordered) or "symbol" (only changes touching the same
	// definition are ordered; needs Tree-sitter support for the language and
	// falls back to file level otherwise).
	Granularity string `yaml:"granularity"`
}

// Command is a verification command. It can be written as a plain string
// or as a mapping with name/run/level.
type Command struct {
	Name    string   `yaml:"name,omitempty"`
	Run     string   `yaml:"run"`
	Level   string   `yaml:"level,omitempty"` // build | lint | test
	Timeout Duration `yaml:"timeout,omitempty"`
}

// UnmarshalYAML accepts either a string or a mapping.
func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		c.Run = n.Value
		return nil
	}
	type raw Command
	var r raw
	if err := n.Decode(&r); err != nil {
		return err
	}
	*c = Command(r)
	return nil
}

// EffectiveLevel returns the declared level or infers one from the command.
func (c Command) EffectiveLevel() string {
	if c.Level != "" {
		return c.Level
	}
	l := strings.ToLower(strings.TrimSpace(c.Run))
	switch {
	case strings.HasPrefix(l, "test ") || strings.HasPrefix(l, "[ "):
		return "build" // shell condition, not a test runner
	case strings.Contains(l, "fmt") || strings.Contains(l, "lint") || strings.Contains(l, "clippy") || strings.Contains(l, " vet"):
		return "lint"
	case testWordRe.MatchString(l):
		return "test"
	default:
		return "build"
	}
}

// DisplayName is a short label for reports.
func (c Command) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Run
}

var testWordRe = regexp.MustCompile(`\b(test|tests|pytest|jest|vitest|mocha|rspec|ctest|spec|check)\b|_test\b|\btest_`)

// Verification configures the verification engine.
type Verification struct {
	Commands     []Command    `yaml:"commands,omitempty"`
	Timeout      Duration     `yaml:"timeout"`
	Regression   Regression   `yaml:"regression"`
	Differential Differential `yaml:"differential"`
	Performance  Performance  `yaml:"performance"`
	// AllowPartial lets partially verified changes proceed (as draft PRs).
	AllowPartial bool `yaml:"allow_partial"`
	// Env lists environment variable names forwarded to verification commands.
	Env []string `yaml:"env,omitempty"`
}

// Regression configures "fails before, passes after" demonstrations.
type Regression struct {
	Enabled bool `yaml:"enabled"`
	// Commands run against the base revision with the new tests applied.
	// Defaults to the test-level verification commands.
	Commands []Command `yaml:"commands,omitempty"`
}

// Differential configures differential testing.
type Differential struct {
	// Command runs with UNDRIFT_UPSTREAM_DIR and UNDRIFT_DOWNSTREAM_DIR set;
	// exit status 0 means equivalent behavior.
	Command string `yaml:"command,omitempty"`
	// Cases run upstream and downstream commands and compare their output.
	Cases []DiffCase `yaml:"cases,omitempty"`
	// Normalize rules applied to both outputs before comparison.
	Normalize Normalize `yaml:"normalize"`
}

// DiffCase is one differential test case.
type DiffCase struct {
	Name       string `yaml:"name"`
	Upstream   string `yaml:"upstream"`
	Downstream string `yaml:"downstream"`
	Input      string `yaml:"input,omitempty"` // file passed on stdin
}

// Normalize describes output normalization for differential testing.
type Normalize struct {
	IgnoreKeys    []string `yaml:"ignore_keys,omitempty"`    // JSON keys removed at any depth
	IgnorePattern []string `yaml:"ignore_pattern,omitempty"` // regexes replaced with "<ignored>"
	SortLines     bool     `yaml:"sort_lines,omitempty"`
	SortArrays    bool     `yaml:"sort_arrays,omitempty"`
}

// Performance configures benchmark comparison.
type Performance struct {
	// Command prints metrics as JSON ({"name": number} or
	// {"metrics": {...}}) or as "name value" lines.
	Command              string   `yaml:"command,omitempty"`
	MaxRegressionPercent float64  `yaml:"max_regression_percent"`
	HigherIsBetter       []string `yaml:"higher_is_better,omitempty"`
}

// PullRequests configures downstream pull request management.
type PullRequests struct {
	AutoCreate     bool     `yaml:"auto_create"`
	AutoMerge      bool     `yaml:"auto_merge"`
	Provider       string   `yaml:"provider"` // auto | github | gitlab | none
	Push           bool     `yaml:"push"`
	DraftOnPartial bool     `yaml:"draft_on_partial"`
	TitlePrefix    string   `yaml:"title_prefix"`
	BranchPrefix   string   `yaml:"branch_prefix"`
	Labels         []string `yaml:"labels,omitempty"`
}

// Watch configures `undrift watch`.
type Watch struct {
	Interval Duration `yaml:"interval"`
}

// Limits bound each run.
type Limits struct {
	MaxChangesPerRun     int     `yaml:"max_changes_per_run"`
	MaxAttemptsPerChange int     `yaml:"max_attempts_per_change"`
	MaxCostUSD           float64 `yaml:"max_cost_usd,omitempty"`
}

// State configures persistence and coordination.
type State struct {
	// Branch used by `undrift state pull/push` to persist the ledger.
	Branch string `yaml:"branch"`
	// Lease backend: "local" (single machine) or "git" (refs on the
	// downstream remote, safe across machines).
	Lease         string   `yaml:"lease"`
	LeaseDuration Duration `yaml:"lease_duration"`
}

// Security configures isolation and least privilege.
type Security struct {
	// Sandbox is a command prefix wrapping agent and verification processes,
	// e.g. ["bwrap", "--unshare-net", ...] or ["firejail", "--net=none"].
	Sandbox []string `yaml:"sandbox,omitempty"`
	// AllowUpstreamExecution permits differential testing to execute
	// upstream code. Disabled by default because upstream is untrusted.
	AllowUpstreamExecution bool `yaml:"allow_upstream_execution"`
}

// Context bounds the task context given to agents.
type Context struct {
	MaxDiffBytes     int  `yaml:"max_diff_bytes"`
	MaxFileBytes     int  `yaml:"max_file_bytes"`
	MaxFiles         int  `yaml:"max_files"`
	UpstreamCheckout bool `yaml:"upstream_checkout"`
}

// Default returns a configuration populated with defaults.
func Default() *Config {
	return &Config{
		Version:    1,
		Downstream: Downstream{Branch: "main", Remote: "origin"},
		Agent: Agent{
			Provider:         "claude",
			MaxAttempts:      3,
			MaxParallelTasks: 1,
			Timeout:          Duration(30 * time.Minute),
		},
		Analysis: Analysis{MinConfidence: 0.6},
		Sync: SyncPolicy{
			SecurityFixes:        true,
			BugFixes:             true,
			Features:             true,
			Performance:          true,
			BreakingChanges:      true,
			Refactoring:          false,
			Dependencies:         false,
			Tests:                true,
			Documentation:        false,
			PreserveArchitecture: true,
		},
		Planner: Planner{MaxGroupSize: 5, Stack: true, Granularity: "file"},
		Verification: Verification{
			Timeout:      Duration(20 * time.Minute),
			Regression:   Regression{Enabled: true},
			Performance:  Performance{MaxRegressionPercent: 5},
			AllowPartial: true,
		},
		PullRequests: PullRequests{
			AutoCreate:     true,
			AutoMerge:      false,
			Provider:       "auto",
			Push:           true,
			DraftOnPartial: true,
			TitlePrefix:    "[Undrift]",
			BranchPrefix:   "undrift/",
		},
		Watch:  Watch{Interval: Duration(time.Hour)},
		Limits: Limits{MaxChangesPerRun: 10, MaxAttemptsPerChange: 3},
		State: State{
			Branch:        "undrift/state",
			Lease:         "local",
			LeaseDuration: Duration(45 * time.Minute),
		},
		Context: Context{
			MaxDiffBytes:     200_000,
			MaxFileBytes:     60_000,
			MaxFiles:         25,
			UpstreamCheckout: true,
		},
	}
}

// Parse decodes configuration bytes on top of the defaults.
func Parse(data []byte) (*Config, error) {
	c := Default()
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileName, err)
	}
	c.normalize()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Load reads the configuration from path. If path is empty, it searches the
// current directory and its parents for undrift.yaml.
func Load(path string) (*Config, error) {
	if path == "" {
		p, err := Find(".")
		if err != nil {
			return nil, err
		}
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, abs)
	}
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, err
	}
	c.Path = abs
	c.Root = filepath.Dir(abs)
	// Local upstream paths are relative to the configuration file.
	for i := range c.Upstreams {
		r := c.Upstreams[i].Repository
		if !strings.Contains(r, "://") && !strings.HasPrefix(r, "git@") && !filepath.IsAbs(r) {
			if p := filepath.Join(c.Root, r); exists(p) {
				c.Upstreams[i].Repository = filepath.Clean(p)
			}
		}
	}
	return c, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ErrNotFound is returned when no configuration file can be located.
var ErrNotFound = errors.New("undrift.yaml not found (run `undrift init`)")

// Find walks up from dir looking for undrift.yaml.
func Find(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(d, FileName)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", ErrNotFound
		}
		d = parent
	}
}

func (c *Config) normalize() {
	if c.Upstream != nil {
		c.Upstreams = append([]Upstream{*c.Upstream}, c.Upstreams...)
		c.Upstream = nil
	}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.Name == "" {
			if i == 0 {
				u.Name = "upstream"
			} else {
				u.Name = fmt.Sprintf("upstream%d", i+1)
			}
		}
		if u.Branch == "" {
			u.Branch = "main"
		}
		if u.OnRewrite == "" {
			u.OnRewrite = "stop"
		}
		if u.HostAPI == "" {
			u.HostAPI = "auto"
		}
	}
	if c.Agent.MaxParallelTasks < 1 {
		c.Agent.MaxParallelTasks = 1
	}
	if c.Agent.MaxAttempts < 1 {
		c.Agent.MaxAttempts = 1
	}
	if c.Limits.MaxAttemptsPerChange < 1 {
		c.Limits.MaxAttemptsPerChange = c.Agent.MaxAttempts
	}
	if c.Planner.MaxGroupSize < 1 {
		c.Planner.MaxGroupSize = 1
	}
	if c.PullRequests.BranchPrefix == "" {
		c.PullRequests.BranchPrefix = "undrift/"
	}
}

// Validate checks semantic constraints.
func (c *Config) Validate() error {
	var errs []string
	if c.Version != 1 {
		errs = append(errs, fmt.Sprintf("unsupported version %d (expected 1)", c.Version))
	}
	if len(c.Upstreams) == 0 {
		errs = append(errs, "upstream.repository is required")
	}
	names := map[string]bool{}
	for _, u := range c.Upstreams {
		if u.Repository == "" {
			errs = append(errs, fmt.Sprintf("upstream %q: repository is required", u.Name))
		}
		if names[u.Name] {
			errs = append(errs, fmt.Sprintf("duplicate upstream name %q", u.Name))
		}
		names[u.Name] = true
		if u.OnRewrite != "stop" && u.OnRewrite != "reconcile" {
			errs = append(errs, fmt.Sprintf("upstream %q: on_rewrite must be stop or reconcile", u.Name))
		}
	}
	switch c.Planner.Granularity {
	case "file", "symbol":
	default:
		errs = append(errs, "planner.granularity must be file or symbol")
	}
	switch c.State.Lease {
	case "local", "git":
	default:
		errs = append(errs, "state.lease must be local or git")
	}
	switch c.PullRequests.Provider {
	case "auto", "github", "gitlab", "none":
	default:
		errs = append(errs, "pull_requests.provider must be auto, github, gitlab or none")
	}
	if c.Analysis.MinConfidence < 0 || c.Analysis.MinConfidence > 1 {
		errs = append(errs, "analysis.min_confidence must be between 0 and 1")
	}
	for name, d := range c.Agents {
		if d.Command == "" {
			errs = append(errs, fmt.Sprintf("agents.%s.command is required", name))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// UpstreamByName returns the upstream with the given name.
func (c *Config) UpstreamByName(name string) (*Upstream, bool) {
	for i := range c.Upstreams {
		if c.Upstreams[i].Name == name {
			return &c.Upstreams[i], true
		}
	}
	return nil, false
}

// StateDir is the absolute path of the .undrift directory.
func (c *Config) StateDir() string { return filepath.Join(c.Root, StateDirName) }

// RunDir holds machine-local runtime data (worktrees, mirrors, locks, logs).
func (c *Config) RunDir() string { return filepath.Join(c.StateDir(), "run") }

// TestCommands returns the verification commands at test level.
func (v Verification) TestCommands() []Command {
	var out []Command
	for _, c := range v.Commands {
		if c.EffectiveLevel() == "test" {
			out = append(out, c)
		}
	}
	return out
}

// Duration is a time.Duration that marshals as a Go duration string.
type Duration time.Duration

// D converts to time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses "30m", "1h30m" or a number of seconds.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v := strings.TrimSpace(n.Value)
	if v == "" || v == "0" {
		*d = 0
		return nil
	}
	if pd, err := time.ParseDuration(v); err == nil {
		*d = Duration(pd)
		return nil
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	return fmt.Errorf("invalid duration %q", v)
}

// MarshalYAML renders the duration as a string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Template renders a starter configuration.
func Template(upstream, branch, baseline, provider string, verify []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# undrift.yaml — see https://github.com/aryehlev/undrift\nversion: 1\n")
	fmt.Fprintf(&b, "upstream:\n  repository: %s\n  branch: %s\n", yamlQuote(upstream), yamlQuote(branch))
	if baseline != "" {
		fmt.Fprintf(&b, "  baseline: %s\n", yamlQuote(baseline))
	}
	b.WriteString("downstream:\n  branch: main\n")
	fmt.Fprintf(&b, "agent:\n  provider: %s\n  max_attempts: 3\n  max_parallel_tasks: 1\n  timeout: 30m\n", provider)
	b.WriteString("sync:\n  security_fixes: true\n  bug_fixes: true\n  features: true\n  refactoring: false\n  preserve_architecture: true\n")
	b.WriteString("verification:\n")
	if len(verify) == 0 {
		b.WriteString("  commands: []   # e.g. [\"cargo build --locked\", \"cargo test --locked\"]\n")
	} else {
		b.WriteString("  commands:\n")
		for _, v := range verify {
			fmt.Fprintf(&b, "    - %s\n", yamlQuote(v))
		}
	}
	b.WriteString("rules:\n  - Preserve existing performance optimizations\n  - Respect intentional architectural deviations\n")
	b.WriteString("pull_requests:\n  auto_create: true\n  auto_merge: false\n")
	b.WriteString("limits:\n  max_changes_per_run: 10\n  max_attempts_per_change: 3\n")
	return b.String()
}

func yamlQuote(s string) string {
	out, _ := yaml.Marshal(s)
	return strings.TrimSpace(string(out))
}
