// Package integrations installs Undrift integration files into the
// downstream repository: CI workflows and agent instructions/skills.
package integrations

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aryehlev/undrift/internal/state"
)

//go:embed templates/*
var templates embed.FS

func tpl(name string) string {
	b, err := templates.ReadFile("templates/" + name)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Guide returns the agent-facing workflow guide.
func Guide() string { return tpl("agent-guide.md") }

// ClaudeSkill returns the Claude Code skill document.
func ClaudeSkill() string {
	return strings.ReplaceAll(tpl("claude-skill.md"), "{{guide}}", strings.TrimPrefix(Guide(), "## Undrift synchronization\n\n"))
}

const (
	markerBegin = "<!-- undrift:begin -->"
	markerEnd   = "<!-- undrift:end -->"
)

// Integration describes one installable integration.
type Integration struct {
	Name        string
	Description string
	install     func(root, agent string) ([]string, error)
}

// secretEnv lists the API key secrets for an agent.
func secretEnv(agent string) string {
	keys := map[string][]string{
		"claude":   {"ANTHROPIC_API_KEY"},
		"codex":    {"OPENAI_API_KEY"},
		"gemini":   {"GEMINI_API_KEY"},
		"opencode": {"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
	}[agent]
	if len(keys) == 0 {
		return "          # add the credentials your agent needs, e.g.\n          # MY_AGENT_KEY: ${{ secrets.MY_AGENT_KEY }}"
	}
	var lines []string
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("          %s: ${{ secrets.%s }}", k, k))
	}
	return strings.Join(lines, "\n")
}

// All lists available integrations.
var All = []Integration{
	{"github-actions", "Scheduled GitHub Actions workflow (.github/workflows/undrift.yml)", func(root, agent string) ([]string, error) {
		content := strings.NewReplacer("{{agent}}", agent, "{{secret_env}}", secretEnv(agent)).Replace(tpl("github-actions.yml"))
		return writeNew(root, ".github/workflows/undrift.yml", content)
	}},
	{"gitlab-ci", "GitLab CI job (undrift.gitlab-ci.yml)", func(root, agent string) ([]string, error) {
		return writeNew(root, "undrift.gitlab-ci.yml", strings.ReplaceAll(tpl("gitlab-ci.yml"), "{{agent}}", agent))
	}},
	{"claude", "Claude Code skill (.claude/skills/undrift/SKILL.md)", func(root, _ string) ([]string, error) {
		return writeNew(root, ".claude/skills/undrift/SKILL.md", ClaudeSkill())
	}},
	{"codex", "Codex instructions (AGENTS.md section)", func(root, _ string) ([]string, error) {
		return upsertSection(root, "AGENTS.md", Guide())
	}},
	{"opencode", "OpenCode instructions (AGENTS.md section)", func(root, _ string) ([]string, error) {
		return upsertSection(root, "AGENTS.md", Guide())
	}},
	{"gemini", "Gemini CLI instructions (GEMINI.md section)", func(root, _ string) ([]string, error) {
		return upsertSection(root, "GEMINI.md", Guide())
	}},
}

// Names lists integration names.
func Names() []string {
	var out []string
	for _, i := range All {
		out = append(out, i.Name)
	}
	sort.Strings(out)
	return out
}

// Install installs an integration into root and returns written paths.
func Install(root, name, agent string) ([]string, error) {
	for _, i := range All {
		if i.Name == name {
			if agent == "" {
				agent = "claude"
			}
			return i.install(root, agent)
		}
	}
	return nil, fmt.Errorf("unknown integration %q (available: %s)", name, strings.Join(Names(), ", "))
}

func writeNew(root, rel, content string) ([]string, error) {
	p := filepath.Join(root, rel)
	if _, err := os.Stat(p); err == nil {
		return nil, fmt.Errorf("%s already exists; remove it to reinstall", rel)
	}
	if err := state.WriteFileAtomic(p, []byte(content)); err != nil {
		return nil, err
	}
	return []string{rel}, nil
}

// upsertSection inserts or replaces the Undrift section of a Markdown file.
func upsertSection(root, rel, body string) ([]string, error) {
	p := filepath.Join(root, rel)
	cur, _ := os.ReadFile(p)
	section := markerBegin + "\n" + strings.TrimSpace(body) + "\n" + markerEnd + "\n"
	s := string(cur)
	if i := strings.Index(s, markerBegin); i >= 0 {
		if j := strings.Index(s[i:], markerEnd); j >= 0 {
			s = s[:i] + section + strings.TrimPrefix(s[i+j+len(markerEnd):], "\n")
		}
	} else {
		if s != "" && !strings.HasSuffix(s, "\n\n") {
			s = strings.TrimRight(s, "\n") + "\n\n"
		}
		s += section
	}
	if err := state.WriteFileAtomic(p, []byte(s)); err != nil {
		return nil, err
	}
	return []string{rel}, nil
}
