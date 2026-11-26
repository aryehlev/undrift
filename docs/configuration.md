# Configuration

Undrift reads a single file, `undrift.yaml`, from the root of the downstream
repository. Commands search the current directory and its parents for it, or
you can pass `--config PATH`. Every field except `upstream.repository` has a
default. [`examples/undrift.yaml`](../examples/undrift.yaml) lists every field
with its default.

A minimal configuration:

```yaml
version: 1
upstream:
  repository: https://github.com/org/original
  baseline: v2.3.0
verification:
  commands:
    - cargo build --locked
    - cargo test --locked
```

## Key decisions

### `upstream.baseline`

The upstream revision that the downstream currently matches. Only changes
after it are tracked. `undrift init` resolves it to a SHA, and uses the
current upstream head if you don't give one. For a partially ported project,
choose the revision you ported from. Changes to code that isn't ported yet
will then be classified against mappings marked `status: missing` or
`unsupported`.

### `sync`

Chooses which classes of change are implemented. Changes whose class is
disabled are recorded as `skipped`, with the policy as the reason. If the
classification's confidence is below `analysis.min_confidence`, they go to
`needs_review` instead. Undrift never silently drops a change it isn't sure
about.

### `verification.commands`

These are the most important setting. Commands run through `sh -c` inside the
task's worktree. Each one has a level: `build`, `lint` or `test`. When you
don't set it, the level is inferred: commands such as `cargo test`, `pytest`
and `make check` are tests; `fmt`, `lint`, `clippy` and `vet` are lint;
anything else is build. After the first failure, later checks are skipped.

How the outcome is decided:

- **verified**: every check passed, at least one was test-level, and (with
  `regression.enabled`) the new tests fail on the base revision and pass after
  the change.
- **partially_verified**: checks passed, but there were no test-level checks,
  no new tests, the new tests didn't fail without the change, or a check was
  skipped. With `allow_partial: true` (the default), the change proceeds as a
  draft PR.
- **unverified**: no checks are configured.
- **failed**: a check failed. The attempt is retried with the failure output
  in context.

Verification commands are trusted configuration. They are always read from
the `undrift.yaml` that Undrift loaded when it started, never from the task
worktree, so an agent can't weaken them.

### `verification.differential`

Runs equivalent inputs through upstream and downstream and compares the
output. Before comparing, `normalize` removes nondeterministic fields
(`ignore_keys`), masks patterns such as timestamps (`ignore_pattern`), and
sorts arrays or lines. Differential testing executes upstream code, so it
only runs with `security.allow_upstream_execution: true`. Combine it with
`security.sandbox`.

### `verification.performance`

Runs the benchmark on the base revision and on the change, then compares the
metrics. A metric that gets worse by more than `max_regression_percent` fails
verification. Metrics are lower-is-better unless they're listed in
`higher_is_better`. The command must print JSON (`{"p99_ms": 4.2}` or
`{"metrics": {...}}`) or `name value` lines.

### `agent` and `agents`

`agent.provider` selects the adapter for `undrift sync` and `undrift run`.
Built-in presets are `claude`, `codex`, `gemini` and `opencode`. Entries under
`agents:` define new adapters or override a preset's `command`, `args`,
`analyze_args`, `prompt`, `output` and `env`. List the resolved command
lines with `undrift agents`.

Agents receive only a minimal environment, plus the variable names you list
in the adapter's `env` or in `agent.env`. A name ending in `*` matches by
prefix, for example `ANTHROPIC_*`.

### `pull_requests`

`provider` can be `auto` (the default), `github`, `gitlab` or `none`. With
`auto`, Undrift detects the host from the downstream remote URL: `github.com`
or the GitHub Enterprise server of the current Actions run means GitHub, and
any host named like GitLab or the instance of the current GitLab CI job means
GitLab. If the remote doesn't identify the host, set `provider` explicitly.

- **GitHub:** PRs are opened with `GITHUB_TOKEN` or `GH_TOKEN`. For GitHub
  Enterprise, set `GITHUB_API_URL`.
- **GitLab:** merge requests are opened with `GITLAB_TOKEN` or `GL_TOKEN`,
  which needs the `api` scope (`CI_JOB_TOKEN` can't create merge requests).
  Drafts use the `Draft:` title prefix. The API URL is derived from the remote
  host; override it with `GITLAB_API_URL`. Self-managed instances work.
- **none:** verified branches are pushed (when `push: true` and a remote
  exists), and they count as merged once the commit reaches
  `downstream.branch`.

The target project is `downstream.repository` (GitHub `owner/name` or a
GitLab `group/subgroup/project` path), or is derived from the remote URL.
`auto_merge` is off by default; when it's on, only fully verified, non-draft
PRs are merged.

### `planner.granularity`

With `file` (the default), any two changes to the same file run in upstream
order. With `symbol`, they're ordered only when they touch the same function,
method or type, so unrelated fixes to one large file can run in parallel. A
change outside every definition (imports, package-level constants), or in a
language without symbol analysis, still orders against every change to that
file.

### `upstream.host_api`

With `auto`, Undrift fetches the upstream PR or MR title, description and
labels when a token is available: `GITHUB_TOKEN` / `GH_TOKEN` for GitHub,
`GITLAB_TOKEN` for GitLab, or `UNDRIFT_UPSTREAM_TOKEN` for either. Labels
such as `security` improve classification. `always` fetches without a token
(public repositories, rate-limited), and `never` disables fetching.

### `state`

- `lease: local` is enough for any number of workers on one machine.
- `lease: git` is for workers on different machines. Leases become refs on
  the remote, updated with compare-and-swap pushes.
- `branch` is the orphan branch that `undrift state pull` / `undrift state
  push` use to persist the ledger between ephemeral CI runs.

### `limits`

`max_changes_per_run`, `max_attempts_per_change` and `max_cost_usd` bound each
run. Cost is counted when the agent reports it; Claude Code's JSON output
does.
