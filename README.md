# adrlog

Decision records and agent reasoning, captured across parallel Claude Code sessions.

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

`git rev-parse --show-toplevel` returns the worktree. State written there scatters across worktrees, and it dies when git removes an unchanged worktree at session end. The shared root does not move, so five sessions write to one place and each one sees the others' records.

**Everything is deterministic.** No LLM call, no embedding, no network. Every answer comes from git and the filesystem, which is also why the whole tool is testable against a scratch repo.

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

**Platforms.** macOS and Linux, on amd64 and arm64. The lock that keeps journal
sequence numbers unique under parallel subagents is `flock`, so the build
excludes Windows rather than shipping an untested substitute for the thing that
guards against duplicate entries. See `internal/flock`.

**Importing it.** This is a command, not a library. Everything sits under
`internal/`, so the module exposes no importable API and none of it is subject
to compatibility promises. Read the records and the journal through the CLI,
where every command takes `--json`.

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

`adrlog` stays completely silent in every repository without one: no journal, no prompt, no output on either stream.

Caution: the default watch list is `internal/** cmd/** migrations/** api/** **/*.tf`. A list that does not fit your repository produces prompts for changes that hold no decision, and those train you to ignore the tool. Set it in `.adrlog/config.json`:

```json
{
  "watch": ["src/**", "migrations/**"],
  "ignore": ["**/*_test.go", "**/testdata/**", "docs/**"],
  "min_files": 2,
  "cooldown_seconds": 900
}
```

Every field is optional. Delete the file and the defaults apply.

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

Hooks stay under a 50ms budget at the 99th percentile. Measured at 200 records, 2000 tracked files and a 10,000-line journal: `post-tool-use` 17ms, `stop` 28ms, `session-start` 17ms.

## The prompt, and why it counts itself

When watched files change and no record exists, the `Stop` hook asks for one. It suppresses itself when fewer than `min_files` changed, when a record is already open, and when the same changed-file set already asked inside the cooldown.

A prompt that gets declined every time, forever, leaves every other measure looking healthy while the tool quietly stops working. So `adrlog` counts them. `adrlog drift` reports the answered share over 30 days. At or below 0.5 it says so, and the reading means one of two things: the watch list is too broad, or the log has become something nobody reads. Both need a person to look, not more automation.

Declining is a complete answer. Run `adrlog ack --none`.

## What it will not do

A record written to satisfy a check is worse than no record. So an accepted record with an empty Alternatives section is a warning addressed to a human, never an error the agent must clear. Failing on it would teach the agent to invent rejected options, and invented alternatives are the worst thing this log can hold.

Also out of scope, deliberately: LLM calls from the binary, embeddings or a vector index, Confluence or Notion or Jira sync, a web UI, cross-repo aggregation, and version control other than git.

## Status

v0.1 covers core record handling and the journal with hooks. Retrieval, the bootstrap audit and drift analysis are designed but not built. They are gated on evidence that the prompt produces records rather than noise. `prd.md` holds the full plan and the reasoning.

`adrlog init`, `adrlog ctx`, `adrlog audit`, `adrlog prune` and `adrlog lint --fix` refuse with a pointer to that gate, rather than returning an empty result that reads like an answer.

Records of the design decisions live in `docs/adr/`, written with the tool.

## Develop

```
make test     # go vet and go test
make check    # the above, plus the cross-worktree verification script
```

`scripts/verify-milestones.sh` builds a scratch repository and checks the properties that unit tests cannot reach: five concurrent worktrees creating records without collision, three parallel subagents appending to one journal, prompt suppression, and the hook latency budget.

## Licence

MIT. See `LICENSE`.
