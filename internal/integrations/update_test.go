package integrations

import (
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "regenerate checked-in integration files")

func TestMain(m *testing.M) {
	flag.Parse()
	if *update {
		if err := os.WriteFile("../../plugins/claude-code/skills/undrift/SKILL.md", []byte(ClaudeSkill()), 0o644); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
