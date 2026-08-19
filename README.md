# adrlog

Keeps decision records and agent reasoning from getting lost across parallel Claude Code sessions.

`adrlog` is one Go binary with no dependencies. It stores two things:

- **Decision records** in `docs/adr/*.md`. Markdown with YAML frontmatter. Durable, reviewed, committed.
- **A journal** in `.adrlog/journal/*.jsonl`. One line per agent turn, written by hooks. Cheap, and it never blocks a turn.

The journal is the part no other ADR tool has. A subagent finishes, returns a summary, and its context is discarded. Whatever it rejected is gone unless something wrote it down first.

## The problem

Three to five Claude Code sessions run in parallel git worktrees, each with subagents. That produces decisions faster than anyone records them:

1. A subagent's reasoning dies with its context.
2. Session B contradicts a decision session A already made, because nothing put A's conclusion in front of B.
3. Sequential ids (`0042-title.md`) collide across worktrees. Two sessions reach for the same integer and conflict over a filename.
4. Six weeks later nobody can tell whether a field exists for a real reason or because someone was in a hurry.

Existing ADR tools address none of 1, 2 or 3. They assume one human, in one checkout, at human speed.

## How it avoids those

**Ids carry a timestamp.** `20260804-143210-store-list-price-and-effective-price`. No counter, so no coordination between worktrees, so no collision.

**State resolves to the shared repository, not the worktree.**

```
root = dirname(git rev-parse --git-common-dir)
```

`git rev-parse --show-toplevel` returns the worktree. Write state there and it scatters. Worse, git deletes an unchanged worktree at session end and takes the state with it. The shared root does not move, so five sessions write to one place and each one reads the others' records.

**Everything is deterministic.** No LLM call, no embedding, no network. Every answer comes from git and the filesystem. That is also why the whole tool tests against a scratch repo instead of needing a live anything.

## Install

```
go install github.com/GuitarWag/adrlog/cmd/adrlog@latest
```

Or pin a release:

```
go install github.com/GuitarWag/adrlog/cmd/adrlog@v0.1.0
```

Or build from a clone:

```
make install-global      # builds to ~/.local/bin/adrlog
```

Check what you have with `adrlog version`.

**Platforms.** macOS and Linux, on amd64 and arm64. `flock` is what stops two
subagents claiming the same journal sequence number, and Windows has no `flock`.
The build excludes it rather than ship a replacement I cannot test. A wrong lock
does not crash. It writes two turns under one number and you find out months
later. See `internal/flock`.

**Importing it.** This is a command, not a library. Every package sits under
`internal/`, so there is no importable API and I promise nothing about
compatibility. Read the records and the journal through the CLI, where every
command takes `--json`.

## Wire up the hooks

Add these to `~/.claude/settings.json`. They then apply to every repository, and stay inert in the ones that have not opted in.

```json
{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command",
      "command": "[ -x ~/.local/bin/adrlog ] || exit 0; exec ~/.local/bin/adrlog hook session-start"}]}],
    "Stop": [{"hooks": [{"type": "command",
      "command": "[ -x ~/.local/bin/adrlog ] || exit 0; exec ~/.local/bin/adrlog hook stop"}]}],
    "SubagentStop": [{"hooks": [{"type": "command", "async": true,
      "command": "[ -x ~/.local/bin/adrlog ] || exit 0; exec ~/.local/bin/adrlog hook subagent-stop"}]}],
    "PreCompact": [{"hooks": [{"type": "command",
      "command": "[ -x ~/.local/bin/adrlog ] || exit 0; exec ~/.local/bin/adrlog hook pre-compact"}]}],
    "PostToolUse": [{"matcher": "Write|Edit", "hooks": [{"type": "command",
      "command": "[ -x ~/.local/bin/adrlog ] || exit 0; exec ~/.local/bin/adrlog hook post-tool-use"}]}]
  }
}
```

## Turn it on for a repository

A repository opts in by having an `.adrlog/` directory:

```
mkdir .adrlog                    # explicit switch
adrlog new "<decision title>"    # writes a record and creates .adrlog/ for you
```

In every repository without one, `adrlog` writes nothing, asks nothing, and prints nothing on either stream.

Set the watch list before you start. The default is `internal/** cmd/** migrations/** api/** **/*.tf`, and a list that does not fit your repository asks about changes that hold no decision. Those prompts train you to ignore the tool, which is the one failure it cannot recover from. Put yours in `.adrlog/config.json`:

```json
{
  "watch": ["src/**", "migrations/**"],
  "ignore": ["**/*_test.go", "**/testdata/**", "docs/**"],
  "min_files": 2,
  "cooldown_seconds": 900
}
```

Every field is optional. Delete the file and the defaults apply.

| Field | Default | Does |
|---|---|---|
| `watch` | `internal/** cmd/** migrations/** api/** **/*.tf` | Paths whose changes can trigger a prompt |
| `ignore` | `**/*_test.go **/testdata/** docs/**` | Paths that never do, applied before `watch` |
| `journal_committed` | `false` | Whether you commit `.adrlog/journal/`. Set your `.gitignore` to match |
| `min_files` | `2` | Fewer watched files than this changed, no prompt |
| `cooldown_seconds` | `900` | The same changed-file set will not prompt twice inside this window |
| `enforce` | `false` | `true` blocks the turn instead of adding context. Capped by `CLAUDE_CODE_STOP_HOOK_BLOCK_CAP` |
| `ctx_limit` | `5` | Records injected by retrieval, once retrieval exists |
| `drift_commit_threshold` | `20` | Commits in `affects` paths before a record counts as stale |
| `drift_window_days` | `90` | How far back drift analysis looks |
| `proposed_stale_days` | `14` | A proposal older than this is unresolved |
| `journal_retention_days` | `90` | What `prune` will drop, once `prune` exists |

The last five configure work that is designed but not built. See
[`docs/future-work.md`](docs/future-work.md).

To turn a repository off again, run `rm -rf .adrlog`. Your records in `docs/adr/` stay.

## Commands

Every command accepts `--json`, so a skill can read the output without scraping text.

| Command | Does |
|---|---|
| `adrlog new <title>` | Writes a record. Flags: `--status --supersedes --depends-on --affects --agent --tags` |
| `adrlog list` | Lists records. Filter with `--status --tag --affects` |
| `adrlog show <id>` | Prints one record |
| `adrlog lint` | Checks the record set. Exits 1 on a real problem |
| `adrlog index` | Regenerates `docs/adr/README.md` |
| `adrlog journal` | Queries turns. `--session --since --agent --grep --export` |
| `adrlog ack --none` | Records that a prompt was answered with no decision |
| `adrlog drift` | Reports the prompt response rate |
| `adrlog hook <event>` | Runs a lifecycle hook |
| `adrlog version` | Reports the build, read from the embedded build info |

## What the hooks do

| Event | Behaviour |
|---|---|
| `SessionStart` | Reports open proposals and any unreadable record. Silent when there is nothing to report. |
| `SubagentStop` | Appends the subagent's closing summary to the journal. |
| `Stop` | Appends the turn, then asks for a record if watched files changed and none exists. |
| `PreCompact` | Appends a turn only. It brackets reasoning that compaction would discard. |
| `PostToolUse` | Lints a record you just wrote and regenerates the index. Exit 2 hands the defect back to Claude. |

The budget is 50ms. Measured against 100 records, 600 tracked files and a 5,000-line journal, the p99 is 12ms for `post-tool-use`, 18ms for `session-start` and 33ms for `stop`. `scripts/verify-milestones.sh` re-measures on every run and fails the build if the median crosses 50ms.

## The prompt, and why it counts itself

When watched files change and no record exists, the `Stop` hook asks for one. It suppresses itself when fewer than `min_files` changed, when a record is already open, and when the same changed-file set already asked inside the cooldown.

This is the failure I worried about most. An agent that declines every prompt, forever, leaves every other measure looking healthy while the log stops growing. Nothing breaks. Nothing turns red. So `adrlog` counts the prompts and the answers, and `adrlog drift` reports the answered share over 30 days.

At or below 0.5 it says so. Either the watch list is too broad and the prompts are noise, or the log has become something nobody reads. Both want a person looking at the config, not more automation.

Declining is a complete answer. Run `adrlog ack --none`.

## What it will not do

A record written to satisfy a check is worse than no record. So an accepted record with an empty Alternatives section warns the human reading the output. It is never an error the agent has to clear. Fail on it and the agent learns to invent rejected options, and invented alternatives are the worst thing this log can hold.

Out of scope on purpose: LLM calls from the binary, embeddings or a vector index, Confluence or Notion or Jira sync, a web UI, cross-repo aggregation, and version control other than git.

## Status

v0.1 covers record handling and the journal with hooks. Retrieval, the bootstrap audit and drift analysis are designed but not built.

The gate on all of it is two weeks of daily use, a prompt response rate above 0.5, and 10 real records written. It is a behavioural gate on purpose. If the response rate sits below 0.5, none of that machinery helps, because a retrieval ranker that surfaces records nobody writes is elaborate nothing. Fix the `watch` list first. [`docs/future-work.md`](docs/future-work.md) holds the designs and what each one has to prove.

Until then `adrlog init`, `adrlog ctx`, `adrlog audit`, `adrlog prune` and `adrlog lint --fix` refuse and point at the gate. They do not return an empty result, which would read like an answer.

The design decisions live in `docs/adr/`, written with the tool.

## Develop

```
make test     # go vet and go test
make check    # the above, plus the cross-worktree verification script
```

`scripts/verify-milestones.sh` builds a scratch repository and checks the properties that unit tests cannot reach: five concurrent worktrees creating records without collision, three parallel subagents appending to one journal, prompt suppression, and the hook latency budget.

## Licence

MIT. See `LICENSE`.
