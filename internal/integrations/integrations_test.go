package integrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginSkillUpToDate(t *testing.T) {
	got, err := os.ReadFile("../../plugins/claude-code/skills/undrift/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != ClaudeSkill() {
		t.Fatal("plugins/claude-code/skills/undrift/SKILL.md is stale; regenerate with `go test ./internal/integrations -update`")
	}
}

func TestInstall(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Project\n\nExisting rules.\n"), 0o644)
	for _, n := range []string{"github-actions", "claude", "codex", "gemini"} {
		if _, err := Install(root, n, "codex"); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	// Re-installing a section is idempotent.
	if _, err := Install(root, "codex", ""); err != nil {
		t.Fatal(err)
	}
	agents, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if strings.Count(string(agents), markerBegin) != 1 || !strings.Contains(string(agents), "Existing rules.") {
		t.Fatalf("AGENTS.md:\n%s", agents)
	}
	wf, _ := os.ReadFile(filepath.Join(root, ".github/workflows/undrift.yml"))
	if !strings.Contains(string(wf), "OPENAI_API_KEY") || !strings.Contains(string(wf), "agent: codex") {
		t.Fatalf("workflow:\n%s", wf)
	}
	if _, err := Install(root, "github-actions", "codex"); err == nil {
		t.Fatal("overwriting an existing workflow should fail")
	}
}
