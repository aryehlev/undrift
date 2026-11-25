# Undrift

**Different code. Same behavior.**

Undrift is an open-source, agent-agnostic synchronization engine. It keeps an
independently evolving implementation (a cross-language port, a heavily
modified fork, an SDK in another language, an alternative implementation of a
protocol) up to date with its upstream project.

Git merge stops working once two codebases no longer share code. Undrift works
on behavior instead: it discovers upstream changes, works out what each one
does, hands it to the coding agent you choose as a structured task, verifies
the result independently, and opens a pull request. It keeps a durable ledger
of what has been synchronized, what was skipped, and why.

> Port once. Stay in sync.

```
upstream commits ──► scan ──► analyze ──► plan ──► agent implements ──► verify ──► PR ──► merged
                     ledger   classify    deps     (Claude, Codex,      build,      draft if
                              + policy    + order   Gemini, OpenCode,   tests,      partially
                                                    any CLI)            regression, verified
                                                                        diff, perf
```

## Highlights

- **Bring your own agent.** Built-in adapters for Claude Code, Codex, Gemini CLI and OpenCode. Any other CLI agent is a few lines of YAML. External agents and loops can drive Undrift themselves through `next / claim / context / verify / complete`, or over MCP.
- **Any language pair.** Undrift doesn't parse code. Mappings between upstream and downstream functionality are semantic and don't assume matching file names or structure.
- **Verification first.** An agent's report is never taken as proof. Undrift re-runs trusted downstream checks: build, existing tests, regression tests (fail before / pass after), differential testing against upstream, and performance thresholds. Results are reported as *verified*, *partially verified* or *unverified*. Partially verified work opens as a draft PR.
- **Persistent memory.** Mappings, intentional deviations, conventions, rejected changes and past failures are kept in human-readable, version-controlled files and given to every agent.
- **Architecture preservation.** Recorded deviations (for example "we use `FuturesUnordered` instead of a task per bidder") are injected into every relevant task. Agents are told to port behavior, not implementation.
- **Durable and resumable.** Each change has a stable identity and a lifecycle in the ledger. Every operation is idempotent. Tasks are leased; crashed workers are recovered when their lease expires. Upstream history rewrites are detected and handled explicitly.
- **Local-first.** No hosted service and no database. State is JSON files. CI runners share state through a git branch, and distributed workers coordinate through compare-and-swap lease refs on the git remote.

## Install

```sh
go install github.com/aryehlev/undrift/cmd/undrift@latest
```

Requires Go 1.22+ and git. Install the agent CLI you want to use (`claude`, `codex`, `gemini`, `opencode`, ...) separately.

## Quick start

Run these commands inside the downstream repository:

```sh
# 1. Point Undrift at the upstream. The baseline is the upstream revision your
#    port currently matches (default: today's upstream head).
undrift init --upstream https://github.com/org/project --baseline v1.4.0 \
             --verify "cargo build --locked" --verify "cargo test --locked"

# 2. Discover, classify and plan upstream changes since the baseline.
undrift scan
undrift analyze            # heuristic; add --agent claude for semantic analysis
undrift plan

# 3. Let an agent implement, verify and open PRs.
export GITHUB_TOKEN=...    # to push branches and open PRs
undrift sync --agent claude

undrift status
```

`undrift run --agent claude` runs all four steps in one go.

## How it works

| Stage | What happens |
|---|---|
| **scan** | Fetches the upstream into a local mirror and records each new *logical* change (first-parent history, so a merged PR or squash commit is one change) in the ledger, with its PR number, tags, patch ID and revert target. Include/exclude/ignore rules are applied, and filtered changes are still recorded with the reason. |
| **analyze** | Classifies each change (security fix, bug fix, feature, performance, breaking change, refactoring, dependency update, test, docs, language-specific, already implemented, not applicable, needs review) with reasoning, evidence and a confidence score, then applies your `sync:` policy. Low-confidence decisions go to human review instead of being skipped. |
| **plan** | Builds tasks and a dependency graph from file overlap, reverts and analysis. Unrelated changes run in parallel. Dependent ones run in order and can *stack* on an unmerged dependency's branch. A blocked change doesn't hold up unrelated work. |
| **sync** | Claims ready tasks under a lease, creates an isolated git worktree for each, and gives the agent a task contract: upstream diff and source, relevant downstream code, mappings, deviations, rules, past decisions and past failed attempts, acceptance criteria and an output schema. |
| **verify** | Re-runs the configured downstream checks on the agent's work. A failure feeds back into the next attempt, up to `max_attempts_per_change`. |
| **PR** | Squashes the work into one commit with `Upstream-Change:` trailers and opens (or updates, never duplicates) a PR. The PR includes upstream links, an explanation, the implementation summary, architectural differences, the verification table, tests added, limitations and confidence. |
| **reconcile** | Detects merged or closed PRs. A change counts as **synchronized only once it is merged**. |

Each change moves through: `discovered → analyzed → planned → in_progress → verified → pr_opened → merged`. Side states are `waiting_dependency`, `blocked`, `failed`, `needs_review`, `already_implemented`, `not_applicable`, `skipped`, `reverted` and `superseded`.

## Two ways to run agents

**Managed:** Undrift launches the agent.

```sh
undrift sync --agent claude      # or codex, gemini, opencode, or your own
undrift sync --change abc123     # one specific change
```

**External:** your agent, loop or orchestrator drives Undrift. Both models share the same state.

```sh
undrift next --format json                    # exit 3 = nothing to do
undrift claim TASK_ID --format json           # → workdir, prompt_file, result_file, token
undrift context TASK_ID                       # Markdown task (or --format json)
undrift verify TASK_ID --token TOKEN
undrift complete TASK_ID --token TOKEN --result result.json
```

The same workflow is available as MCP tools through `undrift mcp`. See [docs/agent-contract.md](docs/agent-contract.md) for the task and result schemas.

### Adding your own agent

```yaml
agents:
  my-agent:
    command: my-agent
    args: ["run", "--instructions", "{{prompt_file}}", "--cwd", "{{workdir}}"]
    prompt: none          # or stdin (default)
    env: ["MY_AGENT_*"]   # only these variables (plus a minimal base set) are passed through
```

The agent works in `{{workdir}}` and writes its result JSON to `$UNDRIFT_RESULT_FILE`. You can also override built-in presets here when an agent CLI changes its flags.

## Human control

```sh
undrift inspect abc123                          # reasoning, evidence, attempts, PR
undrift skip abc123 --reason "Not applicable to the Rust port"
undrift approve abc123                          # override the policy or a review hold
undrift resolve abc123 --as already_implemented --reason "Fixed in #88"
undrift retry abc123                            # fresh attempt budget
undrift history                                 # every decision, who made it, and why
undrift mappings add --upstream auction/auction.go#RunAuction \
                     --downstream src/auction/engine.rs#execute \
                     --relationship "Execute an auction and collect bidder responses"
undrift deviations add --upstream auction/auction.go --downstream src/auction/engine.rs \
                       --reason "FuturesUnordered instead of a task per bidder" \
                       --preserve "Single-task execution" --preserve "Zero-copy request handling"
undrift notes add --kind convention --text "Errors are thiserror enums"
```

Mappings and notes that agents propose are committed on the PR branch, so they reach your main branch only through review. Agents can never overwrite mappings marked `source: human`.

## Running it continuously

- **GitHub Actions:** run `undrift integrations install github-actions`, then add your agent's API key as a repository secret. The workflow uses this repository's [`action.yml`](action.yml). The action builds Undrift, installs the agent CLI, restores the ledger from the `undrift/state` branch, runs the pipeline, persists state and writes a job summary.

  ```yaml
  - uses: aryehlev/undrift@main
    with:
      agent: codex
      max-changes: 10
    env:
      OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}
  ```

- **Claude Code:** run `undrift integrations install claude` to add a project skill. You can also install the plugin, which bundles the skill and the MCP server: `/plugin marketplace add aryehlev/undrift`. For a recurring loop: `/loop 30m Run Undrift, discover upstream changes, implement pending synchronization tasks, verify them, and open pull requests.`
- **Codex / OpenCode / Gemini:** run `undrift integrations install codex` (or `opencode`, `gemini`) to add an Undrift section to `AGENTS.md` / `GEMINI.md`.
- **Anything else:** `undrift run` under cron, GitLab CI (`integrations install gitlab-ci`), Jenkins or a Kubernetes CronJob. `undrift watch` loops locally.

For multiple machines, set `state.lease: git`. Leases then become refs on the downstream remote, updated with compare-and-swap pushes, so workers that share only the git remote never take the same task. Use `undrift state pull` / `undrift state push` to share the ledger through the `undrift/state` branch.

## Security model

Upstream content is untrusted. It can include prompt injection, malicious scripts or poisoned dependencies.

- Upstream text reaches agents only inside `<untrusted-upstream-content>` fences. Attempts to close a fence early are neutralized, and agents are told never to follow instructions inside one.
- Verification commands come only from the downstream `undrift.yaml` that was loaded at startup. Agent edits to `undrift.yaml` and `.undrift/` are discarded before verification.
- Agents and checks run with a minimal environment: `PATH`, `HOME`, locale, proxy and toolchain caches, plus the variables you name explicitly. `GITHUB_TOKEN` and other credentials are never forwarded by default.
- Each task runs in its own worktree, in its own process group, with a timeout. Set `security.sandbox` to wrap every agent and check (for example `["bwrap", ...]`, `["firejail", "--net=none"]` or a `docker run` prefix).
- Undrift never executes upstream code, except in differential testing, which requires `security.allow_upstream_execution: true`.
- Costs are bounded by `limits.max_changes_per_run`, `limits.max_cost_usd`, agent timeouts and attempt limits.
- Upstream PR text in downstream PR bodies is sanitized so it can't `@`-mention people.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | unexpected error |
| 2 | invalid usage |
| 3 | nothing to do |
| 4 | verification failed |
| 5 | task leased by another worker |
| 6 | human review required |
| 7 | configuration missing or invalid |
| 8 | upstream history rewrite detected (`scan --accept-rewrite`) |

Every command accepts `--format json`. Errors in JSON mode are `{"ok": false, "error": "...", "exit_code": N}`.

## Documentation

- [Configuration reference](docs/configuration.md)
- [Agent task contract](docs/agent-contract.md)
- [Architecture and state model](docs/architecture.md)
- [Annotated example configuration](examples/undrift.yaml)

## Status

This is an early MVP. Mapping relies on stored mappings plus file-name heuristics. Tree-sitter, language-server and symbol-index resolvers can plug into `mapping.Resolver`. Pull request management is GitHub-only today; with other hosts, set `pull_requests.provider: none` and Undrift pushes verified branches for review.

## License

Apache 2.0. See [LICENSE](LICENSE).
