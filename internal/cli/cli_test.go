package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/aryehlev/undrift/internal/engine"
	"github.com/aryehlev/undrift/internal/tasks"
)

func TestGlobalsAnywhere(t *testing.T) {
	a := &App{Format: "text"}
	rest, err := a.globals([]string{"claim", "t-1", "--format", "json", "--owner", "me", "-q", "--config=x.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Format != "json" || !a.Quiet || a.ConfigPath != "x.yaml" || strings.Join(rest, " ") != "claim t-1 --owner me" {
		t.Fatalf("format=%s quiet=%v cfg=%s rest=%v", a.Format, a.Quiet, a.ConfigPath, rest)
	}
}

func TestInterspersedFlags(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	owner := fs.String("owner", "", "")
	pos, err := parse(fs, []string{"TASK", "--owner", "me", "extra"})
	if err != nil || *owner != "me" || len(pos) != 2 || pos[0] != "TASK" {
		t.Fatalf("pos=%v owner=%s err=%v", pos, *owner, err)
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[error]int{
		nil:                                     ExitOK,
		engine.ErrNothingToDo:                   ExitNothingToDo,
		fmt.Errorf("x: %w", tasks.ErrLeaseHeld): ExitLeaseConflict,
		fmt.Errorf("x: %w", engine.ErrVerificationFailed): ExitVerificationFailed,
		engine.ErrHistoryRewrite:                          ExitHistoryRewrite,
		usagef("bad"):                                     ExitUsage,
		errors.New("boom"):                                ExitError,
	}
	for err, want := range cases {
		if got := exitCode(err); got != want {
			t.Errorf("exitCode(%v) = %d, want %d", err, got, want)
		}
	}
}

func TestMainJSONErrorAndConfigExitCode(t *testing.T) {
	var out, errb bytes.Buffer
	code := Main([]string{"status", "--format", "json", "--config", "/nonexistent/undrift.yaml"}, &out, &errb)
	if code != ExitConfig {
		t.Fatalf("code %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil || m["ok"] != false {
		t.Fatalf("json error envelope: %s", out.String())
	}
	out.Reset()
	if code := Main([]string{"version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), Version) {
		t.Fatalf("version: %d %s", code, out.String())
	}
	if code := Main([]string{"bogus"}, &out, &errb); code != ExitUsage {
		t.Fatalf("unknown command code %d", code)
	}
}
