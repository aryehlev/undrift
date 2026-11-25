package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aryehlev/undrift/internal/agents"
	"github.com/aryehlev/undrift/internal/analyzer"
	"github.com/aryehlev/undrift/internal/engine"
	"github.com/aryehlev/undrift/internal/integrations"
	"github.com/aryehlev/undrift/internal/report"
	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/tasks"
)

func init() {
	register(command{"init", "Initialize Undrift in the downstream repository", "init --upstream URL [--branch main] [--baseline REV] [--agent claude] [--verify CMD]... [--force]", cmdInit})
	register(command{"scan", "Discover new upstream changes", "scan [--upstream NAME] [--accept-rewrite]", cmdScan})
	register(command{"analyze", "Classify discovered changes", "analyze [--agent NAME] [--force] [--limit N] [CHANGE...]", cmdAnalyze})
	register(command{"plan", "Build the dependency-aware execution plan", "plan", cmdPlan})
	register(command{"sync", "Implement ready tasks with a coding agent", "sync [--agent NAME] [--change REF] [--max-changes N] [--parallel N] [--max-cost USD]", cmdSync})
	register(command{"run", "Scan, analyze, plan and sync in one step", "run [--agent NAME] [--analyze-agent NAME] [--max-changes N] [--parallel N] [--accept-rewrite] [--no-sync]", cmdRun})
	register(command{"next", "Show the next ready task (does not claim it)", "next [--all]", cmdNext})
	register(command{"claim", "Lease a task and prepare its isolated workspace", "claim TASK [--owner NAME] [--lease 45m] [--force]", cmdClaim})
	register(command{"renew", "Extend a task lease", "renew TASK [--token TOKEN] [--lease 45m]", cmdRenew})
	register(command{"release", "Release a task lease without completing it", "release TASK [--token TOKEN]", cmdRelease})
	register(command{"context", "Print the task contract (Markdown prompt or JSON)", "context TASK [--format markdown|json]", cmdContext})
	register(command{"verify", "Run verification for a claimed task", "verify TASK [--token TOKEN]", cmdVerify})
	register(command{"complete", "Report a task result; verifies and opens a PR", "complete TASK [--token TOKEN] (--result FILE | --status STATUS --summary TEXT)", cmdComplete})
	register(command{"publish", "Push and open/update the PR for a verified task", "publish TASK", cmdPublish})
	register(command{"status", "Show synchronization status", "status", cmdStatus})
	register(command{"changes", "List tracked upstream changes", "changes [--status S[,S...]]", cmdChanges})
	register(command{"inspect", "Show details of a change or task", "inspect REF", cmdInspect})
	register(command{"retry", "Retry a failed or parked task", "retry REF", cmdRetry})
	register(command{"skip", "Mark a change as intentionally skipped", "skip REF --reason TEXT", cmdSkip})
	register(command{"approve", "Approve a change for synchronization (overrides policy/review)", "approve REF [--reason TEXT]", cmdApprove})
	register(command{"resolve", "Record a manual outcome for a change", "resolve REF --as already_implemented|not_applicable|merged|skipped --reason TEXT", cmdResolve})
	register(command{"reconcile", "Detect merged/closed PRs and finalize changes", "reconcile", cmdReconcile})
	register(command{"mappings", "List, add, remove or suggest semantic mappings", "mappings [list|add|remove|suggest] ...", cmdMappings})
	register(command{"deviations", "List or add intentional deviations", "deviations [list|add] ...", cmdDeviations})
	register(command{"notes", "List or add project knowledge notes", "notes [list|add --kind KIND --text TEXT [--path GLOB]...]", cmdNotes})
	register(command{"history", "Show historical decisions", "history [--events] [--limit N]", cmdHistory})
	register(command{"report", "Generate a synchronization report", "report [--output FILE]", cmdReport})
	register(command{"watch", "Run the pipeline repeatedly", "watch [--interval 1h] [--agent NAME] [--no-sync]", cmdWatch})
	register(command{"state", "Persist the ledger on a git branch (for CI)", "state pull|push [--branch NAME] [--remote NAME]", cmdState})
	register(command{"integrations", "Install CI and agent integrations", "integrations list | install NAME [--agent NAME]", cmdIntegrations})
	register(command{"agents", "List available agent adapters", "agents", cmdAgents})
	register(command{"schema", "Print the task, result or analysis schema", "schema task|result|analysis", cmdSchema})
	register(command{"version", "Print the version", "version", func(context.Context, *App, []string) (*output, error) {
		return &output{data: map[string]string{"version": Version}, text: "undrift " + Version}, nil
	}})
}

func tw(f func(w *tabwriter.Writer)) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	f(w)
	w.Flush()
	return b.String()
}

func one(pos []string, what string) (string, error) {
	if len(pos) != 1 {
		return "", usagef("expected exactly one %s", what)
	}
	return pos[0], nil
}

func cmdInit(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	var o engine.InitOptions
	var verify multi
	fs.StringVar(&o.Upstream, "upstream", "", "upstream repository URL or path")
	fs.StringVar(&o.Branch, "branch", "main", "upstream branch")
	fs.StringVar(&o.Baseline, "baseline", "", "upstream revision the port corresponds to (default: current head)")
	fs.StringVar(&o.Agent, "agent", "claude", "default agent")
	fs.Var(&verify, "verify", "verification command (repeatable)")
	fs.BoolVar(&o.Force, "force", false, "overwrite existing files")
	fs.BoolVar(&o.StateOnBranch, "state-on-branch", false, "keep the ledger on the state branch instead of committing it")
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	if o.Upstream == "" {
		return nil, usagef("--upstream is required")
	}
	o.Verify = verify
	res, err := engine.Init(ctx, o)
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("Initialized Undrift (baseline %s)\n", res.Baseline[:12])
	for _, c := range res.Created {
		text += "  created " + c + "\n"
	}
	text += "\nNext: edit undrift.yaml (verification commands!), then run `undrift scan && undrift analyze && undrift plan`.\n"
	return &output{data: res, text: text}, nil
}

func cmdScan(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	var o engine.ScanOptions
	fs.StringVar(&o.Upstream, "upstream", "", "only scan this upstream")
	fs.BoolVar(&o.AcceptRewrite, "accept-rewrite", false, "reconcile a rewritten upstream history")
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	res, err := e.Scan(ctx, o)
	out := &output{data: res}
	if res != nil {
		var b strings.Builder
		for _, u := range res.Upstreams {
			fmt.Fprintf(&b, "%s: %d new change(s) up to %s\n", u.Upstream, len(u.Discovered), short(u.Head))
			if len(u.Ignored) > 0 {
				fmt.Fprintf(&b, "  %d recorded as not applicable by include/exclude/ignore rules\n", len(u.Ignored))
			}
			if len(u.Reverted) > 0 {
				fmt.Fprintf(&b, "  reverted before synchronization: %s\n", strings.Join(u.Reverted, ", "))
			}
			if len(u.Releases) > 0 {
				fmt.Fprintf(&b, "  releases: %s\n", strings.Join(u.Releases, ", "))
			}
			if r := u.Rewrite; r != nil {
				fmt.Fprintf(&b, "  HISTORY REWRITE: %s → %s, %d unreachable change(s)", short(r.OldHead), short(r.NewHead), len(r.Dropped))
				if r.Handled {
					fmt.Fprintf(&b, "; superseded %d, need review %d", len(r.Superseded), len(r.NeedReview))
				}
				b.WriteString("\n")
			}
		}
		out.text = b.String()
	}
	return out, err
}

func cmdAnalyze(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	var o engine.AnalyzeOptions
	fs.StringVar(&o.Agent, "agent", "", "agent for semantic analysis (default: analysis.agent or heuristic)")
	fs.BoolVar(&o.Force, "force", false, "re-analyze unplanned changes")
	fs.IntVar(&o.Limit, "limit", 0, "analyze at most N changes")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	o.Changes = pos
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	res, err := e.Analyze(ctx, o)
	if err != nil {
		return nil, err
	}
	text := tw(func(w *tabwriter.Writer) {
		fmt.Fprintln(w, "CHANGE\tCLASS\tCONF\tSTATUS\tTITLE")
		for _, c := range res.Analyzed {
			fmt.Fprintf(w, "%s\t%s\t%.2f\t%s\t%s\n", c.ID, c.Classification, c.Confidence, c.Status, clip(c.Title, 60))
		}
	})
	if len(res.Analyzed) == 0 {
		text = "no changes to analyze\n"
	}
	return &output{data: res, text: text}, nil
}

func planText(p *engine.PlanResult) string {
	var b strings.Builder
	if len(p.Created) > 0 {
		fmt.Fprintf(&b, "Created %d task(s).\n", len(p.Created))
	}
	group := func(title string, ts []engine.PlannedTask) {
		if len(ts) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", title)
		b.WriteString(tw(func(w *tabwriter.Writer) {
			for _, t := range ts {
				extra := t.Reason
				if t.StackOn != "" {
					extra = "stacks on " + t.StackOn
				}
				fmt.Fprintf(w, "  %s\tP%d\t%s\t%s\n", t.ID, t.Priority, clip(t.Title, 56), extra)
			}
		}))
	}
	group("Ready", p.Ready)
	group("Waiting for dependencies", p.Waiting)
	group("Blocked", p.Blocked)
	if b.Len() == 0 {
		return "no open tasks\n"
	}
	return b.String()
}

func cmdPlan(ctx context.Context, a *App, args []string) (*output, error) {
	if _, err := parse(flag.NewFlagSet("plan", flag.ContinueOnError), args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	res, err := e.Plan(ctx)
	if err != nil {
		return nil, err
	}
	return &output{data: res, text: planText(res)}, nil
}

func syncFlags(fs *flag.FlagSet, o *engine.SyncOptions) {
	fs.StringVar(&o.Agent, "agent", "", "agent adapter (default: agent.provider)")
	fs.IntVar(&o.MaxChanges, "max-changes", 0, "maximum changes to process")
	fs.IntVar(&o.Parallel, "parallel", 0, "parallel tasks")
	fs.Float64Var(&o.MaxCostUSD, "max-cost", 0, "stop after spending this many USD")
	fs.StringVar(&o.Owner, "owner", "", "worker identity")
}

func syncText(r *engine.SyncResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, t := range r.Reconciled {
		fmt.Fprintf(&b, "%s: %s → %s (%s)\n", t.Task, t.From, t.To, t.Reason)
	}
	b.WriteString(tw(func(w *tabwriter.Writer) {
		for _, p := range r.Processed {
			fmt.Fprintf(w, "%s\t%s\t%s\n", p.Task, p.Outcome, clip(p.Message, 100))
		}
	}))
	if r.CostUSD > 0 {
		fmt.Fprintf(&b, "cost: $%.2f\n", r.CostUSD)
	}
	if r.Stopped != "" {
		fmt.Fprintf(&b, "stopped: %s\n", r.Stopped)
	}
	return b.String()
}

func syncErr(r *engine.SyncResult, err error) error {
	if err != nil || r == nil {
		return err
	}
	for _, p := range r.Processed {
		if p.Outcome == engine.OutcomeNeedsReview {
			return codedError{ExitNeedsReview, fmt.Errorf("%s needs human review", p.Task)}
		}
	}
	return nil
}

func cmdSync(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	var o engine.SyncOptions
	syncFlags(fs, &o)
	fs.StringVar(&o.Change, "change", "", "only synchronize the task containing this change")
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	res, err := e.Sync(ctx, o)
	return &output{data: res, text: syncText(res)}, syncErr(res, err)
}

func runFlags(fs *flag.FlagSet, o *engine.RunOptions) {
	syncFlags(fs, &o.Sync)
	fs.StringVar(&o.AnalyzeAgent, "analyze-agent", "", "agent for semantic analysis")
	fs.BoolVar(&o.AcceptRewrite, "accept-rewrite", false, "reconcile a rewritten upstream history")
	fs.BoolVar(&o.SkipSync, "no-sync", false, "stop after planning")
}

func runText(r *engine.RunResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	if r.Scan != nil {
		fmt.Fprintf(&b, "scan: %d new change(s)\n", r.Scan.Total)
	}
	if r.Analyze != nil {
		fmt.Fprintf(&b, "analyze: %d change(s)\n", len(r.Analyze.Analyzed))
	}
	if r.Plan != nil {
		fmt.Fprintf(&b, "plan: %d ready, %d waiting, %d blocked\n", len(r.Plan.Ready), len(r.Plan.Waiting), len(r.Plan.Blocked))
	}
	b.WriteString(syncText(r.Sync))
	return b.String()
}

func cmdRun(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var o engine.RunOptions
	runFlags(fs, &o)
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	res, err := e.Run(ctx, o)
	return &output{data: res, text: runText(res)}, syncErr(res.Sync, err)
}

func cmdNext(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("next", flag.ContinueOnError)
	all := fs.Bool("all", false, "list all ready tasks")
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	ready, err := e.Next(ctx, *all)
	if err != nil {
		return nil, err
	}
	type item struct {
		TaskID   string   `json:"task_id"`
		Changes  []string `json:"changes"`
		Priority int      `json:"priority"`
		Title    string   `json:"title"`
		StackOn  string   `json:"stack_on,omitempty"`
		Attempts int      `json:"attempts"`
	}
	var items []item
	for _, c := range ready {
		title := ""
		if ch, err := e.Store.GetChange(c.Task.Changes[0]); err == nil {
			title = ch.Title
		}
		items = append(items, item{c.Task.ID, c.Task.Changes, c.Task.Priority, title, c.Readiness.StackOn, len(c.Task.Attempts)})
	}
	text := tw(func(w *tabwriter.Writer) {
		for _, i := range items {
			fmt.Fprintf(w, "%s\tP%d\t%s\n", i.TaskID, i.Priority, clip(i.Title, 70))
		}
	})
	var data any = items
	if !*all {
		data = items[0]
	}
	return &output{data: data, text: text}, nil
}

func cmdClaim(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	owner := fs.String("owner", "", "worker identity (default host:pid or $UNDRIFT_WORKER)")
	lease := fs.Duration("lease", 0, "lease duration (default state.lease_duration)")
	force := fs.Bool("force", false, "claim even if dependencies are not satisfied")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	id, err := one(pos, "TASK")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	res, err := e.Claim(ctx, id, *owner, *lease, *force)
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("claimed %s (lease until %s)\n  token:   %s\n  workdir: %s\n  prompt:  %s\n  result:  %s\n",
		res.Task.ID, res.ExpiresAt.Local().Format(time.Kitchen), res.Token, res.Workdir, res.PromptFile, res.ResultFile)
	return &output{data: res, text: text}, nil
}

func cmdRenew(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("renew", flag.ContinueOnError)
	token := fs.String("token", os.Getenv("UNDRIFT_TOKEN"), "lease token")
	owner := fs.String("owner", "", "worker identity")
	lease := fs.Duration("lease", 0, "new lease duration")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	id, err := one(pos, "TASK")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	t, err := e.Tasks.Renew(ctx, id, *token, *owner, *lease)
	if err != nil {
		return nil, err
	}
	return &output{data: t.Lease, text: "lease renewed until " + t.Lease.ExpiresAt.Local().Format(time.RFC3339)}, nil
}

func cmdRelease(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	token := fs.String("token", os.Getenv("UNDRIFT_TOKEN"), "lease token")
	owner := fs.String("owner", "", "worker identity")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	id, err := one(pos, "TASK")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	t, err := e.Tasks.Release(ctx, id, *token, *owner)
	if err != nil {
		return nil, err
	}
	return &output{data: t, text: "released " + t.ID}, nil
}

func cmdContext(ctx context.Context, a *App, args []string) (*output, error) {
	pos, err := parse(flag.NewFlagSet("context", flag.ContinueOnError), args)
	if err != nil {
		return nil, err
	}
	id, err := one(pos, "TASK")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	c, err := e.Context(ctx, id)
	if err != nil {
		return nil, err
	}
	return &output{data: c, text: tasks.RenderPrompt(c)}, nil
}

func verifyText(r *state.VerificationReport) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", strings.ToUpper(r.Outcome), r.Summary)
	b.WriteString(tw(func(w *tabwriter.Writer) {
		for _, c := range r.Checks {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.Status, c.Level, clip(c.Name, 50), clip(c.Note, 60))
		}
	}))
	for _, c := range r.Checks {
		if c.Status == "failed" && c.Output != "" {
			fmt.Fprintf(&b, "\n--- %s ---\n%s\n", c.Name, c.Output)
		}
	}
	fmt.Fprintf(&b, "\n%s\n", r.Disclaimer)
	return b.String()
}

func cmdVerify(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	token := fs.String("token", os.Getenv("UNDRIFT_TOKEN"), "lease token")
	owner := fs.String("owner", "", "worker identity")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	id, err := one(pos, "TASK")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	rep, err := e.Verify(ctx, id, *token, *owner)
	return &output{data: rep, text: verifyText(rep)}, err
}

func cmdComplete(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("complete", flag.ContinueOnError)
	token := fs.String("token", os.Getenv("UNDRIFT_TOKEN"), "lease token")
	owner := fs.String("owner", "", "worker identity")
	resultFile := fs.String("result", "", "result JSON file (- for stdin)")
	status := fs.String("status", "", "result status (alternative to --result)")
	summary := fs.String("summary", "", "result summary (with --status)")
	agent := fs.String("agent", "", "name of the agent that did the work")
	confidence := fs.Float64("confidence", 0, "confidence 0..1 (with --status)")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	id, err := one(pos, "TASK")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	var r *state.AgentResult
	switch {
	case *resultFile != "":
		var data []byte
		if *resultFile == "-" {
			data, err = readAll(os.Stdin)
		} else {
			data, err = os.ReadFile(*resultFile)
		}
		if err != nil {
			return nil, err
		}
		if r, err = tasks.ParseResult(data); err != nil {
			return nil, err
		}
	case *status != "":
		r = &state.AgentResult{Status: *status, Summary: *summary, Confidence: *confidence}
	default:
		t, ferr := e.Store.FindTask(id)
		if ferr != nil {
			return nil, ferr
		}
		data, rerr := os.ReadFile(e.TaskDir(t.ID) + "/result.json")
		if rerr != nil {
			return nil, usagef("--result FILE or --status is required (no result file found at the task's result path)")
		}
		if r, err = tasks.ParseResult(data); err != nil {
			return nil, err
		}
	}
	res, err := e.Complete(ctx, id, engine.CompleteOptions{Token: *token, Owner: *owner, Result: r, Agent: *agent})
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("%s: %s\n", res.Outcome, res.Message)
	if res.Report != nil {
		text += verifyText(res.Report)
	}
	var cerr error
	switch res.Outcome {
	case engine.OutcomeRetry, engine.OutcomeFailed:
		if res.Report != nil && res.Report.Outcome == state.OutcomeFailed {
			cerr = codedError{ExitVerificationFailed, fmt.Errorf("%s", res.Message)}
		} else {
			cerr = codedError{ExitError, fmt.Errorf("%s", res.Message)}
		}
	case engine.OutcomeNeedsReview:
		cerr = codedError{ExitNeedsReview, fmt.Errorf("%s", res.Message)}
	}
	return &output{data: res, text: text}, cerr
}

func cmdPublish(ctx context.Context, a *App, args []string) (*output, error) {
	pos, err := parse(flag.NewFlagSet("publish", flag.ContinueOnError), args)
	if err != nil {
		return nil, err
	}
	id, err := one(pos, "TASK")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	t, err := e.Store.FindTask(id)
	if err != nil {
		return nil, err
	}
	pr, err := e.Publish(ctx, t)
	if err != nil {
		return nil, err
	}
	return &output{data: pr, text: fmt.Sprintf("PR #%d %s (%s)", pr.Number, pr.URL, pr.State)}, nil
}

func cmdStatus(ctx context.Context, a *App, args []string) (*output, error) {
	if _, err := parse(flag.NewFlagSet("status", flag.ContinueOnError), args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	s, err := e.Status(ctx)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, u := range s.Upstreams {
		fmt.Fprintf(&b, "%s: %s@%s", u.Name, u.Repository, u.Branch)
		if u.LastScanned != "" {
			fmt.Fprintf(&b, " (scanned through %s, %s)", short(u.LastScanned), u.ScannedAt.Local().Format("2006-01-02 15:04"))
		} else {
			b.WriteString(" (never scanned)")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n%d changes: %d settled, %d remaining\n", s.Total, s.Synced, s.Remaining)
	b.WriteString(tw(func(w *tabwriter.Writer) {
		for _, st := range state.AllStatuses {
			if n := s.Changes[st]; n > 0 {
				fmt.Fprintf(w, "  %s\t%d\n", st, n)
			}
		}
	}))
	list := func(title string, ls []engine.ChangeLine) {
		if len(ls) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", title)
		b.WriteString(tw(func(w *tabwriter.Writer) {
			for _, l := range ls {
				extra := l.Reason
				if l.PR != "" {
					extra = l.PR
				}
				fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", l.ID, l.Status, clip(l.Title, 50), clip(extra, 70))
			}
		}))
	}
	list("Needs attention", s.Attention)
	list("Open PRs", s.OpenPRs)
	list("In flight", s.InFlight)
	if s.CostUSD > 0 {
		fmt.Fprintf(&b, "\nagent cost so far: $%.2f\n", s.CostUSD)
	}
	return &output{data: s, text: b.String()}, nil
}

func cmdChanges(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("changes", flag.ContinueOnError)
	status := fs.String("status", "", "comma-separated statuses")
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	var sts []string
	if *status != "" {
		sts = strings.Split(*status, ",")
	}
	ls, err := e.Changes(ctx, sts)
	if err != nil {
		return nil, err
	}
	text := tw(func(w *tabwriter.Writer) {
		fmt.Fprintln(w, "CHANGE\tREF\tSTATUS\tCLASS\tTITLE")
		for _, l := range ls {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", l.ID, l.Ref, l.Status, l.Classification, clip(l.Title, 60))
		}
	})
	return &output{data: ls, text: text}, nil
}

func cmdInspect(ctx context.Context, a *App, args []string) (*output, error) {
	pos, err := parse(flag.NewFlagSet("inspect", flag.ContinueOnError), args)
	if err != nil {
		return nil, err
	}
	ref, err := one(pos, "REF")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	in, err := e.Inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	c := in.Change
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", c.ID, c.Title)
	fmt.Fprintf(&b, "  upstream:  %s %s", c.Upstream, c.SHA)
	if c.PRURL != "" {
		fmt.Fprintf(&b, " (%s)", c.PRURL)
	}
	fmt.Fprintf(&b, "\n  author:    %s, %s\n  status:    %s", c.Author, c.AuthoredAt.Format("2006-01-02"), c.Status)
	if c.StatusReason != "" {
		fmt.Fprintf(&b, " — %s", c.StatusReason)
	}
	b.WriteString("\n")
	if c.Analysis != nil {
		fmt.Fprintf(&b, "  class:     %s (confidence %.2f, %s)\n  reasoning: %s\n", c.Analysis.Classification, c.Analysis.Confidence, c.Analysis.Analyzer, c.Analysis.Reasoning)
		for _, ev := range c.Analysis.Evidence {
			fmt.Fprintf(&b, "    - %s\n", ev)
		}
	}
	if len(c.DependsOn) > 0 {
		fmt.Fprintf(&b, "  depends:   %s\n", strings.Join(c.DependsOn, ", "))
	}
	fmt.Fprintf(&b, "  files:     %d\n", len(c.Files))
	for i, f := range c.Files {
		if i == 10 {
			fmt.Fprintf(&b, "    ... %d more\n", len(c.Files)-10)
			break
		}
		fmt.Fprintf(&b, "    %s %s\n", f.Status, f.Path)
	}
	if t := in.Task; t != nil {
		fmt.Fprintf(&b, "  task:      %s (%s) branch %s, %d attempt(s)\n", t.ID, t.Status, t.Branch, len(t.Attempts))
		if in.Readiness != nil && t.Status == state.TaskOpen {
			fmt.Fprintf(&b, "  ready:     %v %s\n", in.Readiness.Ready, in.Readiness.Reason)
		}
		for _, at := range t.Attempts {
			fmt.Fprintf(&b, "    attempt %d (%s): %s %s\n", at.Number, at.Agent, at.Outcome, clip(at.Error, 100))
		}
		if t.PR != nil {
			fmt.Fprintf(&b, "  pr:        #%d %s (%s)\n", t.PR.Number, t.PR.URL, t.PR.State)
		}
	}
	for _, d := range c.Decisions {
		fmt.Fprintf(&b, "  decision:  %s %s by %s: %s\n", d.At.Format("2006-01-02"), d.Action, d.By, d.Reason)
	}
	return &output{data: in, text: b.String()}, nil
}

func cmdRetry(ctx context.Context, a *App, args []string) (*output, error) {
	pos, err := parse(flag.NewFlagSet("retry", flag.ContinueOnError), args)
	if err != nil {
		return nil, err
	}
	ref, err := one(pos, "REF")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	t, err := e.Retry(ctx, ref)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return &output{text: ref + " will be re-analyzed"}, nil
	}
	return &output{data: t, text: "task " + t.ID + " reopened"}, nil
}

func decisionCmd(name string, needReason bool, fn func(e *engine.Engine, ctx context.Context, ref, reason string) (*state.Change, error)) func(context.Context, *App, []string) (*output, error) {
	return func(ctx context.Context, a *App, args []string) (*output, error) {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		reason := fs.String("reason", "", "reason for the decision")
		pos, err := parse(fs, args)
		if err != nil {
			return nil, err
		}
		ref, err := one(pos, "REF")
		if err != nil {
			return nil, err
		}
		if needReason && *reason == "" {
			return nil, usagef("--reason is required")
		}
		e, err := a.Engine()
		if err != nil {
			return nil, err
		}
		c, err := fn(e, ctx, ref, *reason)
		if err != nil {
			return nil, err
		}
		return &output{data: c, text: fmt.Sprintf("%s → %s", c.ID, c.Status)}, nil
	}
}

var (
	cmdSkip    = decisionCmd("skip", true, (*engine.Engine).Skip)
	cmdApprove = decisionCmd("approve", false, (*engine.Engine).Approve)
)

func cmdResolve(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	as := fs.String("as", "", "already_implemented | not_applicable | merged | skipped")
	reason := fs.String("reason", "", "reason")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	ref, err := one(pos, "REF")
	if err != nil {
		return nil, err
	}
	if *as == "" || *reason == "" {
		return nil, usagef("--as and --reason are required")
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	c, err := e.Resolve(ctx, ref, state.Status(*as), *reason)
	if err != nil {
		return nil, err
	}
	return &output{data: c, text: fmt.Sprintf("%s → %s", c.ID, c.Status)}, nil
}

func cmdReconcile(ctx context.Context, a *App, args []string) (*output, error) {
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	tr, err := e.Reconcile(ctx)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, t := range tr {
		fmt.Fprintf(&b, "%s: %s → %s (%s)\n", t.Task, t.From, t.To, t.Reason)
	}
	if len(tr) == 0 {
		b.WriteString("no changes\n")
	}
	return &output{data: tr, text: b.String()}, nil
}

func cmdHistory(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	events := fs.Bool("events", false, "include lifecycle events")
	limit := fs.Int("limit", 50, "maximum entries")
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	h, err := e.History(ctx, *events, *limit)
	if err != nil {
		return nil, err
	}
	text := tw(func(w *tabwriter.Writer) {
		for _, x := range h {
			by := x.By
			if by == "" {
				by = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", x.At.Local().Format("2006-01-02 15:04"), x.Change, x.Action, by, clip(x.Detail, 80))
		}
	})
	if len(h) == 0 {
		text = "no decisions recorded\n"
	}
	return &output{data: h, text: text}, nil
}

func cmdReport(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	out := fs.String("output", "", "write the Markdown report to a file")
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	s, err := e.Status(ctx)
	if err != nil {
		return nil, err
	}
	h, _ := e.History(ctx, false, 15)
	md := report.Markdown(report.Data{Status: s, History: h, Now: time.Now()})
	if *out != "" {
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			return nil, err
		}
	}
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" && *out == "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			f.WriteString(md)
			f.Close()
		}
	}
	return &output{data: map[string]any{"status": s, "history": h, "markdown": md}, text: md}, nil
}

func cmdWatch(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	var o engine.RunOptions
	runFlags(fs, &o)
	interval := fs.Duration("interval", 0, "interval between runs (default watch.interval)")
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	err = e.Watch(ctx, *interval, o, func(r *engine.RunResult, err error) {
		if a.Format == "json" {
			b, _ := json.Marshal(map[string]any{"at": time.Now().UTC(), "result": r, "error": errString(err)})
			fmt.Fprintln(a.Stdout, string(b))
			return
		}
		fmt.Fprintf(a.Stdout, "[%s]\n%s", time.Now().Format(time.Kitchen), runText(r))
		if err != nil {
			fmt.Fprintln(a.Stderr, "undrift:", err)
		}
	})
	return nil, err
}

func cmdState(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("state", flag.ContinueOnError)
	branch := fs.String("branch", "", "state branch (default state.branch)")
	remote := fs.String("remote", "", "remote (default downstream.remote)")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	op, err := one(pos, "operation (pull or push)")
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	bs := &state.BranchSync{Repo: e.Repo, Remote: firstNonEmpty(*remote, e.Cfg.Downstream.Remote), Branch: firstNonEmpty(*branch, e.Cfg.State.Branch), Dir: e.Store.Dir()}
	switch op {
	case "pull":
		n, err := bs.Pull(ctx)
		if err != nil {
			return nil, err
		}
		return &output{data: map[string]int{"updated": n}, text: fmt.Sprintf("pulled state from %s (%d record(s) updated)", bs.Branch, n)}, nil
	case "push":
		if err := bs.Push(ctx, "undrift: update synchronization state"); err != nil {
			return nil, err
		}
		return &output{data: map[string]bool{"pushed": true}, text: "pushed state to " + bs.Branch}, nil
	}
	return nil, usagef("unknown state operation %q", op)
}

func cmdIntegrations(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("integrations", flag.ContinueOnError)
	agent := fs.String("agent", "", "agent for CI integrations (default agent.provider)")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) == 0 || pos[0] == "list" {
		text := tw(func(w *tabwriter.Writer) {
			for _, i := range integrations.All {
				fmt.Fprintf(w, "%s\t%s\n", i.Name, i.Description)
			}
		})
		return &output{data: integrations.Names(), text: text}, nil
	}
	if pos[0] != "install" || len(pos) < 2 {
		return nil, usagef("integrations list | install NAME...")
	}
	root := "."
	if e, err := a.Engine(); err == nil {
		root = e.Cfg.Root
		if *agent == "" {
			*agent = e.Cfg.Agent.Provider
		}
	}
	var written []string
	for _, name := range pos[1:] {
		w, err := integrations.Install(root, name, *agent)
		if err != nil {
			return &output{data: written}, err
		}
		written = append(written, w...)
	}
	return &output{data: written, text: "installed: " + strings.Join(written, ", ")}, nil
}

func cmdAgents(ctx context.Context, a *App, args []string) (*output, error) {
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	names := agents.Names(e.Cfg)
	text := ""
	for _, n := range names {
		ag, _ := agents.New(n, e.Cfg)
		if ca, ok := ag.(*agents.CommandAgent); ok {
			text += fmt.Sprintf("%-10s %s\n", n, strings.Join(ca.Argv(agents.Request{Mode: agents.ModeImplement, PromptFile: "PROMPT_FILE"}), " "))
		}
	}
	return &output{data: names, text: text}, nil
}

func cmdSchema(ctx context.Context, a *App, args []string) (*output, error) {
	pos, err := parse(flag.NewFlagSet("schema", flag.ContinueOnError), args)
	if err != nil {
		return nil, err
	}
	which, err := one(pos, "schema name (task, result, analysis)")
	if err != nil {
		return nil, err
	}
	switch which {
	case "result":
		return &output{data: json.RawMessage(stripRanges(tasks.ResultSchema)), text: tasks.ResultSchema}, nil
	case "analysis":
		return &output{data: json.RawMessage(stripRanges(analyzer.AnalysisSchema)), text: analyzer.AnalysisSchema}, nil
	case "task":
		ex := tasks.Contract{Schema: tasks.SchemaVersion, Changes: []tasks.ChangeContext{{}}, Output: tasks.OutputContract{Statuses: tasks.ResultStatuses, Schema: "see `undrift schema result`"}}
		b, _ := json.MarshalIndent(ex, "", "  ")
		return &output{data: ex, text: string(b)}, nil
	}
	return nil, usagef("unknown schema %q", which)
}

// stripRanges turns the documentation schema into valid JSON.
func stripRanges(s string) string { return strings.ReplaceAll(s, "0.0-1.0", "0.0") }

func clip(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func readAll(f *os.File) ([]byte, error) { return io.ReadAll(f) }
