// Package agents adapts Undrift tasks to external coding agents.
//
// Undrift does not implement a coding agent. Every agent is driven through
// the same contract: a Markdown prompt (also written to a file), a JSON task
// file, and a result file the agent writes. Built-in presets exist for
// Claude Code, Codex, Gemini CLI and OpenCode; any other agent is added in
// undrift.yaml under `agents:` without modifying Undrift.
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/proc"
	"github.com/aryehlev/undrift/internal/tasks"
)

// Mode distinguishes implementation from read-only analysis.
type Mode string

// Modes.
const (
	ModeImplement Mode = "implement"
	ModeAnalyze   Mode = "analyze"
)

// Request is one agent invocation.
type Request struct {
	Mode       Mode
	Prompt     string
	PromptFile string
	TaskFile   string
	ResultFile string
	WorkDir    string
	Timeout    time.Duration
	Model      string
	Log        io.Writer
}

// Response is the outcome of an invocation.
type Response struct {
	ExitCode   int           `json:"exit_code"`
	TimedOut   bool          `json:"timed_out"`
	Output     string        `json:"output"`
	ResultJSON []byte        `json:"-"`
	CostUSD    float64       `json:"cost_usd"`
	Duration   time.Duration `json:"duration_ns"`
	IsError    bool          `json:"is_error"`
}

// Agent runs tasks.
type Agent interface {
	Name() string
	Run(ctx context.Context, req Request) (*Response, error)
}

// Presets are the built-in adapter definitions. They can be overridden in
// undrift.yaml (agents.<name>) when agent CLIs change their flags.
var Presets = map[string]config.AgentDef{
	"claude": {
		Command:     "claude",
		Args:        []string{"-p", "--output-format", "json", "--permission-mode", "acceptEdits", "--allowedTools", "Bash,Read,Edit,MultiEdit,Write,Glob,Grep,TodoWrite", "{{model_args}}"},
		AnalyzeArgs: []string{"-p", "--output-format", "json", "--allowedTools", "Read,Glob,Grep", "{{model_args}}"},
		Prompt:      "stdin",
		Output:      "claude-json",
		Env:         []string{"ANTHROPIC_*", "CLAUDE_CODE_*", "CLAUDE_CONFIG_DIR"},
	},
	"codex": {
		Command:     "codex",
		Args:        []string{"exec", "--full-auto", "--skip-git-repo-check", "{{model_args}}", "-"},
		AnalyzeArgs: []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "{{model_args}}", "-"},
		Prompt:      "stdin",
		Output:      "text",
		Env:         []string{"OPENAI_*", "CODEX_*"},
	},
	"gemini": {
		Command:     "gemini",
		Args:        []string{"{{model_args}}", "--yolo", "--prompt", "Read the Undrift task instructions in {{prompt_file}} and follow them exactly."},
		AnalyzeArgs: []string{"{{model_args}}", "--prompt", "Read the Undrift analysis instructions in {{prompt_file}} and follow them exactly. Do not modify any files except the result file named there."},
		Prompt:      "none",
		Output:      "text",
		Env:         []string{"GEMINI_*", "GOOGLE_*"},
	},
	"opencode": {
		Command: "opencode",
		Args:    []string{"run", "{{model_args}}", "Read the Undrift task instructions in {{prompt_file}} and follow them exactly."},
		Prompt:  "none",
		Output:  "text",
		Env:     []string{"OPENCODE_*", "ANTHROPIC_*", "OPENAI_*", "GEMINI_*"},
	},
}

// Names lists available agent names (presets plus configured).
func Names(cfg *config.Config) []string {
	seen := map[string]bool{}
	for n := range Presets {
		seen[n] = true
	}
	for n := range cfg.Agents {
		seen[n] = true
	}
	var out []string
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// New returns the agent with the given name. Configured definitions
// override presets; unknown names fail.
func New(name string, cfg *config.Config) (Agent, error) {
	def, ok := cfg.Agents[name]
	if !ok {
		def, ok = Presets[name]
	} else if p, isPreset := Presets[name]; isPreset {
		def = merge(p, def)
	}
	if !ok {
		return nil, fmt.Errorf("unknown agent %q (available: %s; define custom agents under `agents:` in undrift.yaml)", name, strings.Join(Names(cfg), ", "))
	}
	if def.Prompt == "" {
		def.Prompt = "stdin"
	}
	return &CommandAgent{name: name, def: def, cfg: cfg}, nil
}

func merge(base, over config.AgentDef) config.AgentDef {
	if over.Command != "" {
		base.Command = over.Command
	}
	if len(over.Args) > 0 {
		base.Args = over.Args
	}
	if len(over.AnalyzeArgs) > 0 {
		base.AnalyzeArgs = over.AnalyzeArgs
	}
	if over.Prompt != "" {
		base.Prompt = over.Prompt
	}
	if over.Output != "" {
		base.Output = over.Output
	}
	base.Env = append(base.Env, over.Env...)
	return base
}

// CommandAgent drives an agent through its command-line interface.
type CommandAgent struct {
	name string
	def  config.AgentDef
	cfg  *config.Config
}

// Name implements Agent.
func (a *CommandAgent) Name() string { return a.name }

// Argv expands the argument template for a request.
func (a *CommandAgent) Argv(req Request) []string {
	args := a.def.Args
	if req.Mode == ModeAnalyze && len(a.def.AnalyzeArgs) > 0 {
		args = a.def.AnalyzeArgs
	}
	repl := strings.NewReplacer(
		"{{prompt_file}}", req.PromptFile,
		"{{task_file}}", req.TaskFile,
		"{{result_file}}", req.ResultFile,
		"{{workdir}}", req.WorkDir,
		"{{model}}", req.Model,
		"{{mode}}", string(req.Mode),
	)
	argv := []string{a.def.Command}
	for _, arg := range args {
		switch arg {
		case "{{model_args}}":
			if req.Model != "" {
				argv = append(argv, "--model", req.Model)
			}
			continue
		case "{{prompt}}":
			argv = append(argv, req.Prompt)
			continue
		}
		argv = append(argv, repl.Replace(arg))
	}
	return argv
}

// Run implements Agent.
func (a *CommandAgent) Run(ctx context.Context, req Request) (*Response, error) {
	if req.PromptFile != "" {
		if err := os.WriteFile(req.PromptFile, []byte(req.Prompt), 0o600); err != nil {
			return nil, err
		}
	}
	_ = os.Remove(req.ResultFile)
	names := append(append([]string{}, a.def.Env...), a.cfg.Agent.Env...)
	env := proc.Env(names,
		"UNDRIFT_MODE="+string(req.Mode),
		"UNDRIFT_TASK_FILE="+req.TaskFile,
		"UNDRIFT_PROMPT_FILE="+req.PromptFile,
		"UNDRIFT_RESULT_FILE="+req.ResultFile,
		"UNDRIFT_WORKDIR="+req.WorkDir,
	)
	spec := proc.Spec{
		Argv:    a.Argv(req),
		Dir:     req.WorkDir,
		Env:     env,
		Timeout: req.Timeout,
		Sandbox: a.cfg.Security.Sandbox,
		Log:     req.Log,
	}
	if a.def.Prompt == "stdin" {
		spec.Stdin = strings.NewReader(req.Prompt)
	}
	r, err := proc.Run(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("run agent %s: %w", a.name, err)
	}
	resp := &Response{ExitCode: r.ExitCode, TimedOut: r.TimedOut, Duration: r.Duration}
	text := r.Stdout
	if a.def.Output == "claude-json" {
		var cj struct {
			Result  string  `json:"result"`
			Cost    float64 `json:"total_cost_usd"`
			IsError bool    `json:"is_error"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(r.Stdout)), &cj) == nil {
			text, resp.CostUSD, resp.IsError = cj.Result, cj.Cost, cj.IsError
		}
	}
	resp.Output = proc.Tail(text+"\n"+r.Stderr, 16_000)
	if data, err := os.ReadFile(req.ResultFile); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		resp.ResultJSON = data
	} else {
		key := "status"
		if req.Mode == ModeAnalyze {
			key = "classification"
		}
		resp.ResultJSON = tasks.ExtractJSON(text, key)
	}
	return resp, nil
}
