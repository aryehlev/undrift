# Architecture

Undrift owns the synchronization state and workflow. Coding agents are
interchangeable workers, and the core doesn't depend on any specific agent or
execution environment.

```
cmd/undrift            CLI entry point
internal/
  cli/                 commands, JSON output, exit codes, MCP server
  engine/              orchestration: scan, analyze, plan, claim, verify, complete, sync, reconcile
  config/              undrift.yaml (defaults, validation)
  git/                 thin git CLI wrapper
  upstream/            upstream mirror, logical-change listing, PR/revert/tag/patch-id extraction
  analyzer/            heuristic classification, agent-analysis contract, sync policy
  mapping/             resolves upstream code to downstream components (Resolver interface)
  symbols/             Tree-sitter definitions, touched-symbol detection, excerpts
  memory/              mappings, deviations, notes (version-controlled YAML)
  planner/             dependency graph, grouping, readiness, stacking
  state/               ledger records, file store, state-branch sync
  tasks/               task leasing (local or git), task contract, prompt rendering
  agents/              command adapters (claude, codex, gemini, opencode, custom)
  verifier/            build/test, regression, differential and performance checks
  workspace/           git worktrees
  pullrequest/         PR title/body, GitHub and GitLab providers
  proc/                process execution: timeouts, process groups, env filtering, sandbox
  integrations/        GitHub Actions, GitLab CI, Claude skill, AGENTS.md/GEMINI.md
  report/              Markdown reports
```

## On-disk layout

```
undrift.yaml                     configuration (trusted; never taken from agents)
.undrift/
  memory/                        version-controlled project knowledge
    mappings.yaml
    deviations.yaml
    notes.yaml
  state/                         the ledger (commit it, or keep it on the state branch)
    meta.json                    per-upstream scan cursors
    changes/<change-id>.json     one record per upstream change
    tasks/<task-id>.json         one record per synchronization task
  run/                           machine-local, gitignored
    upstream/<name>.git          bare upstream mirror
    worktrees/<task-id>          isolated downstream workspaces
    upstream-checkouts/<task-id> read-only upstream worktrees
    tasks/<task-id>/             prompt.md, task.json, result.json, attempt-N.log
    locks/
```

Each record is a separate, indented JSON file. That keeps records readable,
diffable and merge-friendly. Every write is atomic (write to a temp file,
then rename) and bumps a `revision`. When state is shared through the
`undrift/state` branch, records are merged per file, and the higher revision
wins.

## Identity and idempotency

- A change's ID is the first 12 characters of its upstream SHA, prefixed with
  the upstream name when there are several upstreams. A rescan never creates
  a duplicate.
- A task's ID is `t-<first change id>`, and its branch is
  `undrift/<first change id>`. Re-planning never duplicates a task.
- Before opening a PR, Undrift looks for an existing PR from the task branch
  and updates it instead of creating another. PR bodies carry an
  `<!-- undrift:task=… -->` marker.
- Verification results are cached by git tree hash, so squashing or
  re-running doesn't repeat work.

## Lifecycle

```
discovered ─► analyzed ─► planned ─► in_progress ─► verified ─► pr_opened ─► merged
     │            │          │            │             │            │
     │            │          ├► waiting_dependency      │            └► needs_review (PR closed)
     │            │          └► blocked (dependency needs attention)
     │            ├► skipped (policy) / not_applicable / already_implemented
     │            └► needs_review (low confidence, ambiguous)
     ├► not_applicable (include/exclude/ignore rules)
     ├► reverted (reverted upstream before it was synchronized)
     └► superseded (dropped or replaced by an upstream history rewrite)
in_progress ─► failed (attempt limit reached) ─► (retry) ─► planned
```

A change is *settled* only when it is `merged`, `already_implemented`,
`not_applicable`, `skipped`, `reverted` or `superseded`. An implementation or
an open PR doesn't count. Dependencies are satisfied by settled changes, or,
with `planner.stack`, by a verified dependency whose branch the new task
builds on.

## Upstream history rewrites

Every scan checks that the previous cursor is still an ancestor of the
upstream head. If it isn't, the scan stops with exit code 8 and lists the
recorded changes that became unreachable. With `--accept-rewrite` (or
`on_rewrite: reconcile`), Undrift rescans from the merge base and matches
unreachable records to new commits by patch ID:

- If the dropped change was already synchronized and an equivalent exists, the
  new commit is marked `already_implemented` and the old one `superseded`.
- If the dropped change was synchronized and no equivalent exists, it is sent
  to `needs_review`, because the downstream may now contain behavior that
  upstream removed.
- A dropped change that was never synchronized is marked `superseded`.

## Concurrency

- Each task runs in its own worktree, so parallel tasks never share files.
- Claiming happens under the store lock, which is an in-process mutex plus an
  `O_EXCL` lock file with stale-lock recovery.
- Leases have an owner, a token and an expiry. Managed runs renew them in the
  background. When a lease expires, another worker can recover the task and
  reuse its worktree.
- With `state.lease: git`, leases also live as `refs/undrift/leases/<task>` on
  the remote. They are acquired, renewed and released with
  `git push --force-with-lease`, a compare-and-swap that git servers perform
  atomically. Local file locks alone aren't enough for distributed workers;
  this gives exclusion with nothing shared but the git remote.
- In GitHub Actions, the workflow's `concurrency` group serializes runs.

## Symbol analysis

When a change is analyzed, Undrift diffs it with zero context lines and
parses the file before and after the change with Tree-sitter. Each changed
line is attributed to the innermost definition that contains it, so a change
inside `Engine.execute` is recorded as `Engine.execute`, not as `Engine`.
Lines outside every definition mark the file as changed at top level. The
result is stored on the change (`files[].symbols`) and used to:

- apply `path#Symbol` mappings only to changes that touch that symbol;
- suggest function-level mappings by matching names across naming
  conventions (`RunAuction`, `run_auction` and `runAuction` all match) against
  an index of downstream definitions;
- give agents only the relevant definitions of files too large to include
  whole, instead of a truncated head;
- with `planner.granularity: symbol`, order only changes that touch the same
  definition.

Grammars are compiled in for Go, Rust, Python, JavaScript, TypeScript/TSX,
Java, C, C++, C#, Ruby, Kotlin, PHP, Swift and Scala. Other languages fall
back to file-level behavior. Tree-sitter needs cgo; a `CGO_ENABLED=0` build
works but leaves symbol analysis off (`undrift version` says which you have).

Tree-sitter comes before language servers on purpose. It needs no build of
either project, so it works on the bare upstream mirror, and it is fast
enough to run on every scan. A language server could add references and
types for deeper dependency planning; it would plug into the same
`mapping.Resolver` interface as an optional per-language resolver.

## Extending

- **Agents**: add an `agents:` entry in `undrift.yaml`. No code changes needed.
- **Mapping resolvers**: implement `mapping.Resolver`, for example with
  Tree-sitter, an LSP or a symbol index.
- **PR providers**: implement `pullrequest.Provider`, for example for Gitea,
  Forgejo or Bitbucket. GitHub and GitLab are built in.
- **Lease backends**: implement `tasks.LeaseBackend`, for example with etcd,
  Redis or DynamoDB.
