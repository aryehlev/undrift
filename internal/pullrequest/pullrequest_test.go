package pullrequest

import (
	"strings"
	"testing"

	"github.com/aryehlev/undrift/internal/state"
)

func TestTitleAndMarker(t *testing.T) {
	c := &state.Change{Title: "Fix auction timeout", PRNumber: 1842, SHA: "abcdef1234567"}
	if got := Title("[Undrift]", []*state.Change{c}); got != "[Undrift] Port fix auction timeout from upstream #1842" {
		t.Fatalf("title %q", got)
	}
	body := Body(BodyInput{Task: &state.Task{ID: "t-x", Changes: []string{"x", "y"}}, Changes: []*state.Change{c}})
	id, changes, ok := ParseMarker(body)
	if !ok || id != "t-x" || len(changes) != 2 {
		t.Fatalf("marker %q %v %v", id, changes, ok)
	}
	if !strings.Contains(sanitize("thanks @octocat"), "@​") {
		t.Fatal("mentions must be neutralized")
	}
}

func TestTitleStripsConventionalPrefix(t *testing.T) {
	c := &state.Change{Title: "fix(auction): Bidder timeout", SHA: "abcdef1234567"}
	if got := Title("[Undrift]", []*state.Change{c}); got != "[Undrift] Port bidder timeout from upstream abcdef1" {
		t.Fatalf("title %q", got)
	}
}
