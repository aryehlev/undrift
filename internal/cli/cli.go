// Package cli implements the undrift command line.
//
// Every command used by automation supports `--format json` and returns
// documented exit codes (see ExitCodes).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/engine"
	"github.com/aryehlev/undrift/internal/tasks"
)

// Version is set at build time.
var Version = "0.1.0-dev"

// Exit codes.
const (
	ExitOK                 = 0
	ExitError              = 1
	ExitUsage              = 2
	ExitNothingToDo        = 3
	ExitVerificationFailed = 4
	ExitLeaseConflict      = 5
	ExitNeedsReview        = 6
	ExitConfig             = 7
	ExitHistoryRewrite     = 8
)

// ExitCodes documents exit codes for `undrift help exit-codes`.
var ExitCodes = []struct {
	Code    int
	Meaning string
}{
	{ExitOK, "success"},
	{ExitError, "unexpected error"},
	{ExitUsage, "invalid command line usage"},
	{ExitNothingToDo, "nothing to do (no pending or ready work)"},
	{ExitVerificationFailed, "verification failed"},
	{ExitLeaseConflict, "task is leased by another worker"},
	{ExitNeedsReview, "human review required"},
	{ExitConfig, "configuration missing or invalid"},
	{ExitHistoryRewrite, "upstream history rewrite detected (rerun scan with --accept-rewrite)"},
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// codedError carries an explicit exit code.
type codedError struct {
	code int
	err  error
}

func (c codedError) Error() string { return c.err.Error() }
func (c codedError) Unwrap() error { return c.err }

func exitCode(err error) int {
	var ce codedError
	var ue usageError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ce):
		return ce.code
	case errors.As(err, &ue):
		return ExitUsage
	case errors.Is(err, engine.ErrNothingToDo):
		return ExitNothingToDo
	case errors.Is(err, engine.ErrVerificationFailed):
		return ExitVerificationFailed
	case errors.Is(err, tasks.ErrLeaseHeld):
		return ExitLeaseConflict
	case errors.Is(err, engine.ErrNeedsReview):
		return ExitNeedsReview
	case errors.Is(err, engine.ErrHistoryRewrite):
		return ExitHistoryRewrite
	case errors.Is(err, config.ErrNotFound), strings.HasPrefix(err.Error(), "invalid configuration"), strings.HasPrefix(err.Error(), "parse undrift.yaml"):
		return ExitConfig
	}
	return ExitError
}

// App holds global options for one invocation.
type App struct {
	ConfigPath string
	Format     string // text | json | markdown
	Quiet      bool
	Stdout     io.Writer
	Stderr     io.Writer

	cfg *config.Config
	eng *engine.Engine
}

// output is what a command produces.
type output struct {
	data any
	text string
}

type command struct {
	name    string
	summary string
	usage   string
	run     func(ctx context.Context, a *App, args []string) (*output, error)
}

var commands []command

func register(c command) { commands = append(commands, c) }

func lookup(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	a := &App{Format: "text", Stdout: stdout, Stderr: stderr}
	if v := os.Getenv("UNDRIFT_FORMAT"); v != "" {
		a.Format = v
	}
	rest, err := a.globals(args)
	if err != nil {
		fmt.Fprintln(stderr, "undrift:", err)
		return ExitUsage
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		topic := ""
		if len(rest) > 1 {
			topic = rest[1]
		}
		fmt.Fprint(stdout, help(topic))
		return ExitOK
	}
	cmd, ok := lookup(rest[0])
	if !ok {
		fmt.Fprintf(stderr, "undrift: unknown command %q (run `undrift help`)\n", rest[0])
		return ExitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out, err := cmd.run(ctx, a, rest[1:])
	return a.finish(cmd, out, err)
}

func (a *App) finish(cmd command, out *output, err error) int {
	code := exitCode(err)
	if a.Format == "json" {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		switch {
		case err != nil && out != nil && out.data != nil:
			_ = enc.Encode(map[string]any{"ok": false, "error": err.Error(), "exit_code": code, "result": out.data})
		case err != nil:
			_ = enc.Encode(map[string]any{"ok": false, "error": err.Error(), "exit_code": code})
		case out != nil && out.data != nil:
			_ = enc.Encode(out.data)
		default:
			_ = enc.Encode(map[string]any{"ok": true})
		}
		if err != nil && !a.Quiet {
			fmt.Fprintln(a.Stderr, "undrift:", err)
		}
		return code
	}
	if out != nil && out.text != "" {
		fmt.Fprint(a.Stdout, out.text)
		if !strings.HasSuffix(out.text, "\n") {
			fmt.Fprintln(a.Stdout)
		}
	}
	if err != nil {
		if code == ExitNothingToDo {
			fmt.Fprintln(a.Stderr, "undrift: nothing to do")
		} else {
			fmt.Fprintln(a.Stderr, "undrift:", err)
		}
		var ue usageError
		if errors.As(err, &ue) && cmd.usage != "" {
			fmt.Fprintf(a.Stderr, "usage: undrift %s\n", cmd.usage)
		}
	}
	return code
}

// globals extracts global flags from anywhere in args.
func (a *App) globals(args []string) ([]string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		val := func() (string, error) {
			if j := strings.Index(arg, "="); j >= 0 {
				return arg[j+1:], nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch {
		case arg == "--":
			rest = append(rest, args[i:]...)
			return rest, nil
		case arg == "--config" || strings.HasPrefix(arg, "--config=") || arg == "-c":
			v, err := val()
			if err != nil {
				return nil, err
			}
			a.ConfigPath = v
		case arg == "--format" || strings.HasPrefix(arg, "--format="):
			v, err := val()
			if err != nil {
				return nil, err
			}
			switch v {
			case "text", "json", "markdown", "md":
			default:
				return nil, fmt.Errorf("unknown format %q (text, json, markdown)", v)
			}
			if v == "md" {
				v = "markdown"
			}
			a.Format = v
		case arg == "--json":
			a.Format = "json"
		case arg == "-q" || arg == "--quiet":
			a.Quiet = true
		default:
			rest = append(rest, arg)
		}
	}
	return rest, nil
}

// Engine loads configuration and opens the engine.
func (a *App) Engine() (*engine.Engine, error) {
	if a.eng != nil {
		return a.eng, nil
	}
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return nil, err
	}
	var logw io.Writer = a.Stderr
	if a.Quiet {
		logw = io.Discard
	}
	e, err := engine.Open(cfg, logw)
	if err != nil {
		return nil, err
	}
	a.cfg, a.eng = cfg, e
	return e, nil
}

// parse parses flags interspersed with positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usagef("%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type multi []string

func (m *multi) String() string { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func help(topic string) string {
	if topic == "exit-codes" {
		var b strings.Builder
		b.WriteString("Exit codes:\n")
		for _, e := range ExitCodes {
			fmt.Fprintf(&b, "  %d  %s\n", e.Code, e.Meaning)
		}
		return b.String()
	}
	if c, ok := lookup(topic); ok {
		return fmt.Sprintf("undrift %s\n\n%s\n", c.usage, c.summary)
	}
	var b strings.Builder
	b.WriteString("Undrift — different code, same behavior.\n\nKeeps independently evolving implementations in sync with their upstream projects.\n\nUsage: undrift [--config FILE] [--format text|json] [--quiet] <command> [args]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-13s %s\n", c.name, c.summary)
	}
	b.WriteString("\nRun `undrift help <command>` for usage, `undrift help exit-codes` for exit codes.\n")
	return b.String()
}
