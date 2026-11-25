---
name: undrift
description: Synchronize this repository with its upstream project using Undrift. Use when asked to sync, port or catch up with upstream changes, or to process Undrift tasks.
---

# Undrift

{{guide}}

### Recurring operation

To keep the port in sync continuously from Claude Code, run for example:

```
/loop 30m Run Undrift: discover upstream changes, implement pending synchronization tasks, verify them, and complete them so Undrift opens pull requests.
```

Persistent operation does not require a Claude Code loop: the same state is
used by `undrift sync --agent claude`, GitHub Actions and any scheduler.
