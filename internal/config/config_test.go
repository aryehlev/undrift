package config

import (
	"strings"
	"testing"
	"time"
)

const specExample = `
version: 1
upstream:
  repository: https://github.com/org/original
  branch: main
  baseline: abc123
downstream:
  branch: main
agent:
  provider: claude
  max_attempts: 3
  max_parallel_tasks: 3
  timeout: 30m
sync:
  security_fixes: true
  bug_fixes: true
  features: true
  refactoring: false
  preserve_architecture: true
verification:
  commands:
    - cargo fmt --check
    - cargo build --locked
    - cargo clippy
    - cargo test --locked
    - name: integration
      run: ./scripts/it.sh
      level: test
  differential:
    command: ./scripts/compare.sh
  performance:
    command: ./scripts/benchmark.sh
    max_regression_percent: 5
rules:
  - Preserve existing performance optimizations
pull_requests:
  auto_create: true
  auto_merge: false
watch:
  interval: 1h
limits:
  max_changes_per_run: 10
  max_attempts_per_change: 3
`

func TestParseSpecExample(t *testing.T) {
	c, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Upstreams) != 1 || c.Upstreams[0].Name != "upstream" || c.Upstreams[0].Baseline != "abc123" {
		t.Fatalf("upstreams = %+v", c.Upstreams)
	}
	if c.Agent.Timeout.D() != 30*time.Minute || c.Agent.MaxParallelTasks != 3 {
		t.Fatalf("agent = %+v", c.Agent)
	}
	if len(c.Verification.Commands) != 5 {
		t.Fatalf("commands = %+v", c.Verification.Commands)
	}
	levels := []string{"lint", "build", "lint", "test", "test"}
	for i, cmd := range c.Verification.Commands {
		if cmd.EffectiveLevel() != levels[i] {
			t.Errorf("command %q level = %s, want %s", cmd.Run, cmd.EffectiveLevel(), levels[i])
		}
	}
	// Defaults survive partial documents.
	if !c.Sync.Tests || c.Sync.Documentation || c.PullRequests.Provider != "github" || c.State.Lease != "local" {
		t.Fatalf("defaults lost: %+v %+v", c.Sync, c.PullRequests)
	}
	if c.Verification.Performance.MaxRegressionPercent != 5 {
		t.Fatal("performance threshold")
	}
}

func TestCommandLevels(t *testing.T) {
	for run, want := range map[string]string{
		"test -f engine.rs": "build", "go test ./...": "test", "pytest -q": "test", "npm run lint": "lint",
		"cargo build --locked": "build", "make check": "test", "[ -d src ]": "build",
	} {
		if got := (Command{Run: run}).EffectiveLevel(); got != want {
			t.Errorf("%q: %s want %s", run, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	_, err := Parse([]byte("version: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "upstream.repository") {
		t.Fatalf("expected missing upstream error, got %v", err)
	}
	_, err = Parse([]byte("version: 1\nupstream: {repository: x}\nstate: {lease: zookeeper}\n"))
	if err == nil || !strings.Contains(err.Error(), "state.lease") {
		t.Fatalf("expected lease error, got %v", err)
	}
}

func TestTemplateRoundTrip(t *testing.T) {
	src := Template("https://example.com/a.git", "main", "deadbeef", "codex", []string{"go test ./..."})
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	if c.Agent.Provider != "codex" || c.Upstreams[0].Baseline != "deadbeef" || len(c.Verification.Commands) != 1 {
		t.Fatalf("round trip: %+v", c)
	}
}

func TestExampleConfigParses(t *testing.T) {
	c, err := Load("../../examples/undrift.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.MaxParallelTasks != 3 || len(c.Verification.Differential.Cases) != 1 || c.Agents["aider"].Command != "aider" {
		t.Fatalf("%+v", c)
	}
}
