package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryehlev/undrift/internal/config"
)

func TestPresetArgv(t *testing.T) {
	cfg := config.Default()
	a, err := New("claude", cfg)
	if err != nil {
		t.Fatal(err)
	}
	argv := a.(*CommandAgent).Argv(Request{Mode: ModeImplement, Model: "opus"})
	if argv[0] != "claude" || !strings.Contains(strings.Join(argv, " "), "--model opus") {
		t.Fatalf("argv %v", argv)
	}
	argv = a.(*CommandAgent).Argv(Request{Mode: ModeAnalyze})
	if strings.Contains(strings.Join(argv, " "), "Edit") {
		t.Fatalf("analysis must be read-only: %v", argv)
	}
	if _, err := New("nope", cfg); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func TestCustomAgentRunAndEnvIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SECRET_PROD_TOKEN", "do-not-leak")
	t.Setenv("MY_AGENT_KEY", "ok")
	cfg := config.Default()
	cfg.Agents = map[string]config.AgentDef{"mine": {
		Command: "sh",
		Args:    []string{"-c", `cat > /dev/null; echo "{\"status\":\"implemented\",\"summary\":\"$MY_AGENT_KEY-${SECRET_PROD_TOKEN:-none}\"}" > "$UNDRIFT_RESULT_FILE"`},
		Env:     []string{"MY_AGENT_*"},
	}}
	a, err := New("mine", cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Run(context.Background(), Request{Mode: ModeImplement, Prompt: "hi", PromptFile: filepath.Join(dir, "p.md"), ResultFile: filepath.Join(dir, "r.json"), WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.ResultJSON), "ok-none") {
		t.Fatalf("result %s (output %s)", resp.ResultJSON, resp.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "p.md")); err != nil {
		t.Fatal("prompt file not written")
	}
}
