# Agent task contract

Undrift does not implement a coding agent. It gives any agent the same
contract: a task to read, a workspace to work in, and a result to write. The
contract is versioned (`schema: undrift.task/v1`).

## Files

When a task is claimed, Undrift creates:

| Path | Purpose |
|---|---|
| `workdir` | An isolated git worktree of the downstream repo, on branch `undrift/<change-id>`. Work **only** here. |
| `prompt_file` | The task rendered as Markdown instructions. |
| `task_file` | The same task as JSON (`undrift context TASK --format json`). |
| `result_file` | Where the agent writes its result JSON. |
| `upstream_checkout` | A read-only worktree of upstream at the change (if enabled). Do not run it. |

Managed agents get these paths as environment variables: `UNDRIFT_WORKDIR`,
`UNDRIFT_PROMPT_FILE`, `UNDRIFT_TASK_FILE`, `UNDRIFT_RESULT_FILE` and
`UNDRIFT_MODE` (`implement` or `analyze`).

## Task contents

- **changes**: one entry per upstream change, with repository, SHA, commit and PR
  URLs, title, message, PR body, author, classification, analysis (reasoning,
  behavior change, suggested tests), revert target, changed files, the diff,
  and the post-change content of upstream files. All upstream text is
  untrusted.
- **downstream**: workdir, branch, base, the branch this task is stacked on
  (if any), and the relevant downstream files resolved through mappings.
- **mappings** / **suggested_mappings** / **unmapped_upstream_files**
- **deviations**: intentional architectural differences that must be preserved.
- **notes**: architecture decisions, conventions, unsupported features,
  performance constraints, human feedback and learned facts.
- **rules**: project rules from `undrift.yaml`.
- **previous_decisions**: skips, rejections and resolutions on related changes.
- **previous_attempts**: earlier attempts on this task, including failed check
  output, so the agent can learn from them.
- **required_behavior** and **acceptance_criteria**
- **verification**: the commands Undrift will run, and whether regression,
  differential and performance checks apply.
- **output**: the result file path and schema.
- **security**: rules for handling untrusted content.

Print the full JSON shape with `undrift schema task`.

## Result

```json
{
  "status": "implemented",
  "summary": "Ported the bidder timeout: execute() now drops responses after the deadline.",
  "files_changed": ["src/auction/engine.rs"],
  "tests_added": ["tests/auction_timeout.rs"],
  "architectural_notes": "Kept FuturesUnordered; the deadline is applied with tokio::time::timeout_at on the stream.",
  "limitations": ["Upstream also logs a metric; downstream has no metrics system yet."],
  "confidence": 0.85,
  "mappings": [
    {"upstream": ["auction/auction.go#RunAuction"], "downstream": ["src/auction/engine.rs#execute"],
     "kind": "function", "relationship": "Execute an auction and collect bidder responses"}
  ],
  "notes": ["Bidder deadlines are enforced in engine.rs, not in the HTTP client."],
  "commit_message": "optional"
}
```

| `status` | Meaning | What Undrift does |
|---|---|---|
| `implemented` | Downstream code was changed. | Discards edits to `undrift.yaml` and `.undrift/`, records proposed memory, verifies, squashes, pushes and opens a PR. If verification fails, the attempt is retried with the failure in context. |
| `already_implemented` | The behavior already exists downstream. | Settles the change with the summary as evidence. If confidence is below `analysis.min_confidence`, sends it to human review instead. |
| `not_applicable` | The change doesn't apply downstream. | Same as `already_implemented`. |
| `needs_human` | Unsafe to decide automatically. | Parks the task for human review. |
| `failed` | The agent couldn't do it. | Counts a failed attempt and retries until the limit is reached. |

If an agent prints its result instead of writing the file, Undrift takes the
last JSON object with a `status` key from its output. If it reports
`implemented` but changed nothing, the attempt fails.

## External agent loop

```sh
while task=$(undrift next --format json); do
  id=$(echo "$task" | jq -r .task_id)
  claim=$(undrift claim "$id" --format json)
  token=$(echo "$claim" | jq -r .token)
  workdir=$(echo "$claim" | jq -r .workdir)
  # ... let your agent read `undrift context $id` and edit $workdir ...
  undrift verify "$id" --token "$token" || true   # iterate until green
  undrift complete "$id" --token "$token" --result "$(echo "$claim" | jq -r .result_file)"
done   # `undrift next` exits 3 when there is nothing to do
```

For work that outlasts the lease (default 45m), renew it with
`undrift renew TASK --token TOKEN`. If a worker dies, its lease expires and
the task becomes claimable again. The existing worktree is reused.

## MCP

`undrift mcp` serves the same workflow over stdio as these tools:
`undrift_status`, `undrift_refresh`, `undrift_next`, `undrift_claim`,
`undrift_context`, `undrift_verify`, `undrift_complete` and `undrift_skip`.

```json
{ "mcpServers": { "undrift": { "command": "undrift", "args": ["mcp"] } } }
```
