// Package verifier runs layered verification of downstream changes:
//
//	Level 1  compilation / lint          (verification.commands, level build|lint)
//	Level 2  existing tests              (verification.commands, level test)
//	Level 3  upstream regression tests   (new tests must fail on the base revision
//	                                      and pass after the change)
//	Level 4  differential testing        (compare upstream and downstream outputs)
//	Level 5  performance verification    (compare benchmark metrics to base)
//
// Commands always come from trusted downstream configuration, never from
// upstream content or agent output. The outcome distinguishes verified,
// partially verified and unverified changes; passing checks are never
// presented as proof of complete behavioral parity.
package verifier

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/git"
	"github.com/aryehlev/undrift/internal/proc"
	"github.com/aryehlev/undrift/internal/state"
)

// Disclaimer is attached to every report.
const Disclaimer = "Passing checks demonstrate the configured verification criteria only; they are not proof of complete behavioral parity with upstream."

// Input describes what to verify.
type Input struct {
	WorkDir     string
	Commit      string
	BaseRef     string
	TestFiles   []string
	UpstreamDir string
	// NewWorktree creates a temporary worktree at rev for baselines.
	NewWorktree func(ctx context.Context, name, rev string) (string, func(), error)
	Log         func(format string, a ...any)
}

// Verifier runs verification.
type Verifier struct {
	Cfg      config.Verification
	Security config.Security
}

func (v *Verifier) logf(in Input, format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

func (v *Verifier) run(ctx context.Context, dir string, cmd config.Command, extra ...string) (*proc.Result, error) {
	timeout := cmd.Timeout.D()
	if timeout == 0 {
		timeout = v.Cfg.Timeout.D()
	}
	return proc.Run(ctx, proc.Spec{
		Argv:    proc.Shell(cmd.Run),
		Dir:     dir,
		Env:     proc.Env(v.Cfg.Env, append([]string{"UNDRIFT_VERIFY=1"}, extra...)...),
		Timeout: timeout,
		Sandbox: v.Security.Sandbox,
	})
}

func checkFrom(name, level, command string, r *proc.Result, err error) state.CheckResult {
	c := state.CheckResult{Name: name, Level: level, Command: command}
	switch {
	case err != nil:
		c.Status, c.ExitCode, c.Note = "error", -1, err.Error()
	case r.TimedOut:
		c.Status, c.ExitCode, c.Note = "failed", -1, "timed out"
	case r.ExitCode != 0:
		c.Status, c.ExitCode = "failed", r.ExitCode
	default:
		c.Status = "passed"
	}
	if r != nil {
		c.Duration = r.Duration
		if c.Status != "passed" {
			c.Output = proc.Tail(strings.TrimSpace(r.Stdout+"\n"+r.Stderr), 4000)
		}
	}
	return c
}

// Run performs all configured verification layers.
func (v *Verifier) Run(ctx context.Context, in Input) *state.VerificationReport {
	rep := &state.VerificationReport{Commit: in.Commit, StartedAt: state.Now(), Disclaimer: Disclaimer}
	failed := false

	// Levels 1 and 2.
	for _, cmd := range v.Cfg.Commands {
		lvl := cmd.EffectiveLevel()
		if failed {
			rep.Checks = append(rep.Checks, state.CheckResult{Name: cmd.DisplayName(), Level: lvl, Command: cmd.Run, Status: "skipped", Note: "earlier check failed"})
			continue
		}
		v.logf(in, "verify: %s", cmd.Run)
		r, err := v.run(ctx, in.WorkDir, cmd)
		c := checkFrom(cmd.DisplayName(), lvl, cmd.Run, r, err)
		rep.Checks = append(rep.Checks, c)
		if c.Status != "passed" {
			failed = true
		}
	}

	// Level 3.
	if v.Cfg.Regression.Enabled && !failed {
		rep.Regression = v.regression(ctx, in, rep)
	}

	// Level 4.
	if !failed && (v.Cfg.Differential.Command != "" || len(v.Cfg.Differential.Cases) > 0) {
		for _, c := range v.differential(ctx, in) {
			rep.Checks = append(rep.Checks, c)
			if c.Status == "failed" || c.Status == "error" {
				failed = true
			}
		}
	}

	// Level 5.
	if !failed && v.Cfg.Performance.Command != "" {
		c, metrics := v.performance(ctx, in)
		rep.Checks = append(rep.Checks, c)
		rep.Metrics = metrics
		if c.Status == "failed" || c.Status == "error" {
			failed = true
		}
	}

	rep.Outcome, rep.Summary = outcome(rep, v.Cfg, failed)
	rep.FinishedAt = state.Now()
	return rep
}

func outcome(rep *state.VerificationReport, cfg config.Verification, failed bool) (string, string) {
	var passed, skipped int
	tests := false
	for _, c := range rep.Checks {
		switch c.Status {
		case "passed":
			passed++
			if c.Level == "test" {
				tests = true
			}
		case "skipped":
			skipped++
		}
	}
	if failed {
		var names []string
		for _, c := range rep.Checks {
			if c.Status == "failed" || c.Status == "error" {
				names = append(names, c.Name)
			}
		}
		return state.OutcomeFailed, "failed: " + strings.Join(names, ", ")
	}
	if passed == 0 {
		return state.OutcomeUnverified, "no verification checks are configured; the change is unverified"
	}
	var gaps []string
	if !tests {
		gaps = append(gaps, "no test-level checks")
	}
	if cfg.Regression.Enabled {
		switch {
		case rep.Regression == nil || len(rep.Regression.Tests) == 0:
			gaps = append(gaps, "no regression tests were added")
		case !rep.Regression.Demonstrated:
			gaps = append(gaps, "new tests did not fail before the change")
		}
	}
	if skipped > 0 {
		gaps = append(gaps, fmt.Sprintf("%d check(s) skipped", skipped))
	}
	if len(gaps) > 0 {
		return state.OutcomePartiallyVerified, fmt.Sprintf("%d check(s) passed; partial: %s", passed, strings.Join(gaps, "; "))
	}
	msg := fmt.Sprintf("%d check(s) passed", passed)
	if rep.Regression != nil && rep.Regression.Demonstrated {
		msg += "; regression tests fail before and pass after the change"
	}
	return state.OutcomeVerified, msg
}

func (v *Verifier) regression(ctx context.Context, in Input, rep *state.VerificationReport) *state.Regression {
	reg := &state.Regression{Tests: in.TestFiles}
	if len(in.TestFiles) == 0 {
		reg.Note = "no downstream tests were added or modified"
		return reg
	}
	cmds := v.Cfg.Regression.Commands
	if len(cmds) == 0 {
		cmds = v.Cfg.TestCommands()
	}
	if len(cmds) == 0 {
		reg.Note = "no test commands configured"
		return reg
	}
	reg.PassedAfter = true
	for _, c := range rep.Checks {
		if c.Level == "test" && c.Status != "passed" {
			reg.PassedAfter = false
		}
	}
	if in.NewWorktree == nil || in.BaseRef == "" {
		reg.Note = "no base revision available"
		return reg
	}
	dir, cleanup, err := in.NewWorktree(ctx, "regression", in.BaseRef)
	if err != nil {
		reg.Note = "could not create base worktree: " + err.Error()
		return reg
	}
	defer cleanup()
	r := git.Open(dir)
	var applied []string
	for _, f := range in.TestFiles {
		if _, err := r.Run(ctx, "checkout", in.Commit, "--", f); err == nil {
			applied = append(applied, f)
		}
	}
	if len(applied) == 0 {
		reg.Note = "test files could not be applied to the base revision"
		return reg
	}
	v.logf(in, "verify: regression (new tests against base %s)", short(in.BaseRef))
	for _, cmd := range cmds {
		res, err := v.run(ctx, dir, cmd)
		if err == nil && (res.ExitCode != 0 || res.TimedOut) {
			reg.FailedBefore = true
			break
		}
	}
	reg.Demonstrated = reg.FailedBefore && reg.PassedAfter
	if !reg.FailedBefore {
		reg.Note = "the new tests also pass without the change; they may not exercise the fixed behavior"
	}
	rep.Checks = append(rep.Checks, state.CheckResult{
		Name: "regression", Level: "regression", Status: map[bool]string{true: "passed", false: "skipped"}[reg.Demonstrated],
		Note: fmt.Sprintf("failed_before=%v passed_after=%v %s", reg.FailedBefore, reg.PassedAfter, reg.Note),
	})
	return reg
}

func (v *Verifier) differential(ctx context.Context, in Input) []state.CheckResult {
	d := v.Cfg.Differential
	if !v.Security.AllowUpstreamExecution {
		return []state.CheckResult{{Name: "differential", Level: "differential", Status: "skipped",
			Note: "differential testing executes upstream code; enable security.allow_upstream_execution to run it"}}
	}
	if in.UpstreamDir == "" {
		return []state.CheckResult{{Name: "differential", Level: "differential", Status: "skipped", Note: "no upstream checkout available"}}
	}
	env := []string{"UNDRIFT_UPSTREAM_DIR=" + in.UpstreamDir, "UNDRIFT_DOWNSTREAM_DIR=" + in.WorkDir}
	var out []state.CheckResult
	if d.Command != "" {
		v.logf(in, "verify: differential command")
		r, err := v.run(ctx, in.WorkDir, config.Command{Run: d.Command}, env...)
		out = append(out, checkFrom("differential", "differential", d.Command, r, err))
	}
	for _, c := range d.Cases {
		name := "differential:" + c.Name
		v.logf(in, "verify: %s", name)
		up, err := v.runCase(ctx, in.UpstreamDir, c.Upstream, c.Input, in.WorkDir, env)
		if err != nil {
			out = append(out, state.CheckResult{Name: name, Level: "differential", Status: "error", Note: "upstream: " + err.Error()})
			continue
		}
		down, err := v.runCase(ctx, in.WorkDir, c.Downstream, c.Input, in.WorkDir, env)
		if err != nil {
			out = append(out, state.CheckResult{Name: name, Level: "differential", Status: "error", Note: "downstream: " + err.Error()})
			continue
		}
		nu, nd := Normalize(up, d.Normalize), Normalize(down, d.Normalize)
		cr := state.CheckResult{Name: name, Level: "differential", Status: "passed"}
		if nu != nd {
			cr.Status = "failed"
			cr.Output = proc.Tail(DiffSummary(nu, nd), 4000)
		}
		out = append(out, cr)
	}
	return out
}

func (v *Verifier) runCase(ctx context.Context, dir, cmd, input, inputBase string, env []string) (string, error) {
	spec := proc.Spec{
		Argv: proc.Shell(cmd), Dir: dir, Env: proc.Env(v.Cfg.Env, env...),
		Timeout: v.Cfg.Timeout.D(), Sandbox: v.Security.Sandbox,
	}
	if input != "" {
		p := input
		if !filepath.IsAbs(p) {
			p = filepath.Join(inputBase, p)
		}
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		defer f.Close()
		spec.Stdin = f
	}
	r, err := proc.Run(ctx, spec)
	if err != nil {
		return "", err
	}
	if r.TimedOut {
		return "", fmt.Errorf("timed out")
	}
	return r.Stdout, nil
}

func (v *Verifier) performance(ctx context.Context, in Input) (state.CheckResult, []state.Metric) {
	p := v.Cfg.Performance
	cmd := config.Command{Run: p.Command}
	check := state.CheckResult{Name: "performance", Level: "performance", Command: p.Command}
	v.logf(in, "verify: performance (current)")
	cur, err := v.run(ctx, in.WorkDir, cmd)
	if err != nil || cur.ExitCode != 0 {
		return checkFrom("performance", "performance", p.Command, cur, err), nil
	}
	curM, err := ParseMetrics(cur.Stdout)
	if err != nil {
		check.Status, check.Note = "error", "parse metrics: "+err.Error()
		return check, nil
	}
	if in.NewWorktree == nil || in.BaseRef == "" {
		check.Status, check.Note = "skipped", "no base revision to compare against"
		return check, nil
	}
	dir, cleanup, err := in.NewWorktree(ctx, "perf-base", in.BaseRef)
	if err != nil {
		check.Status, check.Note = "error", err.Error()
		return check, nil
	}
	defer cleanup()
	v.logf(in, "verify: performance (base)")
	base, err := v.run(ctx, dir, cmd)
	if err != nil || base.ExitCode != 0 {
		check.Status, check.Note = "skipped", "benchmark failed on base revision"
		return check, nil
	}
	baseM, err := ParseMetrics(base.Stdout)
	if err != nil {
		check.Status, check.Note = "skipped", "parse base metrics: "+err.Error()
		return check, nil
	}
	metrics, regressed := Compare(baseM, curM, p.MaxRegressionPercent, p.HigherIsBetter)
	check.Status = "passed"
	if len(regressed) > 0 {
		check.Status = "failed"
		check.Note = fmt.Sprintf("regression over %.1f%%: %s", p.MaxRegressionPercent, strings.Join(regressed, ", "))
	}
	return check, metrics
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// Elapsed formats a duration for reports.
func Elapsed(d time.Duration) string { return d.Round(100 * time.Millisecond).String() }
