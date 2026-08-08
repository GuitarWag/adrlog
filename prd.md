# dlog: decision tracking for parallel agent sessions

**Status:** draft
**Author:** Wagner
**Date:** 2026-08-04
**Name:** `dlog` is provisional. It collides with a few Go logging packages, so rename before publishing anything.

---

## 0. Scope decision

The unique value of this tool ships in the first two milestones: collision-free ids, worktree-correct shared state, and the per-turn journal. No existing tool has the journal, and it is roughly a weekend of Go. Everything after M2 — retrieval, audit, drift — re-implements things adr-kit already does, deterministically instead of with an LLM, and is where the effort actually lives.

Therefore:

- **M1 and M2 are committed.** They are v0.1 and they stand on their own. This resolves the build-versus-adopt question for the differentiator: adopting adr-kit would not provide it.
- **M3 through M5 are gated**, each on evidence defined in section 13, the most important being proof that the nudge produces records rather than becoming wallpaper (section 8.1). If adr-kit's retrieval and audit hold up in a parallel trial on one repo, the correct move at that gate is to port v0.1's journal and ids onto adr-kit and not build M3+ at all.

A PRD that hedges "maybe don't build this" in its risk table has not made a decision. This section is the decision.

---

## 1. Summary

A single Go binary that records architecture decisions and agent reasoning across parallel Claude Code sessions. It serves two audiences from one data store: humans reading why the codebase is shaped this way, and agents that need the relevant subset of prior decisions injected before they write code.

Two tiers:

- **ADRs** in `docs/adr/*.md`, one file per decision, markdown with YAML frontmatter. Durable, reviewed, committed.
- **Journal** in `.dlog/journal/*.jsonl`, one line per agent turn, written automatically. Ephemeral by nature, cheap, never blocks anything.

The binary is also the hook implementation. `dlog hook stop`, `dlog hook subagent-stop`, and so on dispatch on subcommand, so four Claude Code lifecycle hooks cost one build artifact and roughly 2ms of process startup each.

---

## 2. Problem

Running 3 to 5 Claude Code sessions in parallel git worktrees, with subagents inside them, produces decisions faster than anyone documents them, and destroys the reasoning behind those decisions as a matter of course:

1. A subagent finishes, returns a summary to the parent, and its context is discarded. Whatever it considered and rejected is gone.
2. Session B makes a choice that contradicts an accepted decision from session A, because nothing put session A's conclusion in front of it.
3. Sequential ADR numbering (`0042-title.md`) collides across worktrees. Two sessions both reach for the next free integer and produce a merge conflict over a filename.
4. Six weeks later nobody can tell whether a field exists for a compliance reason or because someone was in a hurry.

Existing ADR tooling addresses none of 1, 2, or 3. It assumes a human author working alone, in one checkout, at human speed.

---

## 3. Prior art

`rvdbreemen/adr-kit` is the closest existing tool: pre-commit verification with an optional LLM judge, a staleness Guardian on a two-tier cadence, a bootstrap audit, and relevance-scored context retrieval. It works across Cursor, Codex, and Cowork. It is also pre-1.0 with one GitHub star.

What we take from it:

- The bootstrap audit idea. A decision log seeded from an existing codebase is worth far more than one that starts empty, because the decisions already in force are the ones agents violate.
- Relevance-scored retrieval instead of dumping the whole log into context. Injecting the last twelve ADRs works at 20 records and is noise at 200.
- Drift detection as a first-class feature.

What we reject:

- Sequential ids. Non-negotiable given worktrees.
- LLM-based judging and drift detection. Non-deterministic, costs tokens per commit, and produces disagreements you cannot debug. Everything in this tool is deterministic and explainable. Where judgment is genuinely needed, the agent supplies it through a skill, not the binary.
- ADRs as the only tier.

What neither has:

- A per-turn journal captured from `SubagentStop`. This is the differentiator and the reason to build rather than adopt.

---

## 4. Goals

| # | Goal | Measured by |
|---|---|---|
| G1 | No id or filename collisions across concurrent worktrees | 5 concurrent sessions creating ADRs produce zero conflicts |
| G2 | Subagent reasoning survives context discard | `dlog journal --session X` recovers a rejected alternative that never reached an ADR |
| G3 | Agents see the decision that governs the code they are editing | Recall of hand-labelled relevant records above 0.85; median under 2k tokens injected; fewer than `ctx_limit` records returned when scores fall off (section 9) |
| G4 | Chains are queryable, not prose | `supersedes` / `depends_on` traversable, reciprocity enforced by lint |
| G5 | The nudge produces records, not noise | Response rate: ADRs created or explicit no-decision replies within one turn of a nudge, above 0.5 over a rolling 30 days (section 8.1). Volume stays sane: median growth under 8 ADRs per week |
| G6 | Bootstrap is not a blank page | `dlog audit` on the target repo surfaces at least 15 decision-shaped artifacts with file evidence |
| G7 | Hooks are invisible when there is nothing to say | Zero stdout on a turn with no watched changes; p99 hook latency under 50ms |

**What has no metric, on purpose.** Filler — a plausible-sounding record whose Alternatives section was fabricated to satisfy a check — is the worst failure mode and is undetectable deterministically, because generating plausible text is precisely what the agent is good at. No lint rule fixes this, and a lint rule *rejecting* empty Alternatives would make it worse by training the agent to invent rejected options. So lint treats an empty Alternatives section on an accepted record as a warning addressed to the human reviewer, never a failure the agent must clear. The defenses that remain are behavioral, not mechanical: a high bar in the skill, a narrow `watch` list, a non-blocking default, and a human occasionally reading the log.

### Non-goals

- LLM calls of any kind from the binary.
- Embeddings or a vector index. Lexical scoring plus path matching is sufficient at the scale of a few hundred records, and it is debuggable.
- Confluence, Notion, or Jira sync.
- A web UI. `docs/adr/README.md` with a mermaid graph renders in GitHub, which is enough.
- Cross-repo aggregation. Later, if ever.
- Non-git version control.

---

## 5. Core concepts

### 5.1 Identifiers

`YYYYMMDD-HHMMSS-slug`, for example `20260804-143210-store-list-price-and-effective-price`. Filename stem equals the id. Slug is the first six words of the title, lowercased.

Collision requires the same second and the same slug. `dlog new` appends `-2` if the path exists, and `dlog lint` catches duplicates regardless.

Sortable, needs no coordination between worktrees, and readable at a glance.

### 5.2 Shared state under worktrees

`git rev-parse --show-toplevel` returns the *worktree* path. Journals written there scatter across worktrees and die when a worktree with no changes is auto-removed at session end.

All state resolves against the shared repository instead:

```
root = dirname(git rev-parse --git-common-dir)
```

Verified: from inside a worktree this returns the main checkout, not the worktree. `.dlog/` therefore always lives in one place regardless of which of five sessions is writing.

Sharing the root reintroduces shared mutable state, so each piece of it gets an explicit concurrency posture rather than an assumption:

| State | Written by | Posture |
|---|---|---|
| `docs/adr/<id>.md` | one session each | Safe by construction: timestamp ids mean no two sessions target the same path |
| `.dlog/journal/<session>.jsonl` | its own session | Safe by construction: one writer per file |
| `.dlog/state/` | every session | **Keyed per worktree**: `state/<worktree>/`. A nudge fingerprint from worktree A must not suppress a nudge in worktree B, because their changed-file sets differ. Sharing this state would be wrong, not just racy |
| `docs/adr/README.md` | every session, on any ADR write | **Idempotent last-write-wins by design**: generated deterministically from the full ADR set, so whichever regeneration runs last is correct. A stale index between writes is tolerated; the pre-commit hook regenerates before commit, which is the point that matters |
| Supersede back-link | the superseding session | **Edits a file another session may hold dirty.** `dlog new --supersedes` warns when the target file has uncommitted modifications, and lint catches broken reciprocity in every session afterward, so a lost write is detected rather than silent |

### 5.3 The graph

Four link types, all frontmatter arrays of ids:

- `depends_on`: this decision builds on that conclusion.
- `supersedes` / `superseded_by`: reciprocal, maintained automatically by `dlog new --supersedes`.
- `journal_refs`: `session#seq` pointers into the journal, linking a record to the turns that produced it.

A DAG, not a tree. Parallel sessions converge and multiple parents are the normal case.

### 5.4 `affects` globs

The schema addition that makes three features work at once:

```yaml
affects:
  - internal/pricing/**
  - migrations/*_pricing.sql
```

- **Retrieval**: changed files matched against `affects` is a far stronger relevance signal than text similarity, and it is deterministic.
- **Drift**: churn in `affects` paths since the ADR date is measurable from git alone, no LLM required.
- **Lint**: an `affects` glob matching zero files means the code moved or died, so the record is orphaned.

Cost: the globs need maintaining, and more importantly they are written at the moment of least certainty — by the agent, at creation time, before the decision has fully landed in code. A wrong `affects` at birth corrupts everything downstream at once: `journal_refs` inference, retrieval, and drift all key off it, and their errors will be correlated. Two mitigations:

- **Birth check.** `dlog new` invoked with a session compares the supplied globs against the union of `changed_files` in that session's journal. Globs matching none of the session's changed files, or session changes in watched paths matched by no glob, produce a warning in the command output that the agent sees and can act on immediately. Cheap, deterministic, and it runs at the only moment ground truth is available.
- **Rot check.** `dlog lint` warns on globs matching zero files in the current tree; `dlog drift` reports the same as orphans.

---

## 6. Data model

### 6.1 ADR frontmatter

```yaml
---
id: 20260804-143210-store-list-price-and-effective-price
title: Store list price and effective price as separate fields
status: accepted          # proposed | accepted | rejected | superseded | reverted
date: 2026-08-04
author: subagent:schema-reviewer
branch: feat/tiered-pricing
worktree: app-pricing
session: 4f2a91c
affects:
  - internal/pricing/**
supersedes: []
superseded_by: []
depends_on: [20260801-091500-append-only-event-log]
journal_refs: [4f2a91c#12, 4f2a91c#15]
tags: [pricing, billing]
---

## Context
## Decision
## Alternatives considered
## Consequences
```

Required: `id`, `title`, `status`, `date`. Everything else optional but populated by `dlog new`.

Body sections are fixed and checked by lint. An accepted record with an empty Alternatives section is a *warning*, surfaced to humans in `lint` and `drift` output — not a failure the agent must clear. Failing on it would train the agent to fabricate rejected options, which is worse than an honest empty section (see section 4).

Parse a deliberately small YAML subset by hand (scalars, inline arrays, block sequences). No YAML dependency. This keeps the binary dependency-free and the format easy to reimplement — and creates a specific hazard: an editor or teammate reflowing frontmatter into legal-but-unsupported YAML (folded scalars, anchors, flow mappings) would silently drop the record from every index, retrieval, and lint pass. So *unparseable or partially-parseable frontmatter is itself a first-class lint failure*, reported with the offending line, never a silent skip. A record the parser cannot read is a broken record, not an invisible one.

### 6.2 Journal entry

One JSONL line per turn, appended to `.dlog/journal/<session>.jsonl`:

```json
{"seq":12,"ts":"2026-08-04T14:32:10Z","event":"SubagentStop","session":"4f2a91c",
 "agent_type":"schema-reviewer","agent_id":"a1","prompt_id":"p7",
 "branch":"feat/tiered-pricing","worktree":"app-pricing","head":"2605c19",
 "changed_files":["internal/pricing/store.go"],
 "summary":"Chose a tombstone column over hard delete; hard delete breaks the append-only event log requirement in ADR 20260801-091500.",
 "transcript":"/path/to/agent/transcript.jsonl"}
```

`seq` is monotonic per session so `journal_refs` can point at a specific turn. `summary` comes from the hook payload's `last_assistant_message`, truncated to 1200 bytes. `transcript` is a path, not content, so entries stay small.

Append-only. Never rewritten. Concurrent appends from parallel sessions land in different files, so no locking is needed.

Whether `.dlog/journal/` is committed is a per-repo choice, not a default we impose. `dlog init` asks, records the answer as `journal_committed` in config, and manages `.gitignore` to match. Committed journals make agent reasoning reviewable in PRs at the cost of noise in every diff. For repos that leave them out, `dlog journal --export <session>` writes a single readable markdown trace suitable for pasting into a PR comment, so a specific session can be shared without committing all of them.

### 6.3 Inferring `journal_refs`

`dlog new` populates `journal_refs` automatically rather than requiring the caller to pass turn pointers it does not know.

Selection, in order:

1. Session comes from `CLAUDE_CODE_SESSION_ID`, or `--session` when invoked outside a hook.
2. Candidate entries are those in that session's journal whose `changed_files` overlap the new ADR's `affects` globs. This reuses the glob machinery from section 5.4, so the same signal that drives retrieval also links records to the turns that produced them.
3. If `affects` is empty or nothing overlaps, fall back to every entry since the previous ADR created in this session, or since session start if there is none.
4. Cap at 10 entries, most recent first.

Inference will sometimes attach a turn that had nothing to do with the decision. That is acceptable because the field is advisory: it tells a reader where to look, not what is true. `--no-refs` opts out, and lint checks only that the pointers resolve to real entries, never that they are apt.

### 6.4 Config

`.dlog/config.json`, all fields optional:

```json
{
  "watch": ["internal/**", "cmd/**", "migrations/**", "api/**", "**/*.tf"],
  "ignore": ["**/*_test.go", "**/testdata/**", "docs/**"],
  "journal_committed": false,
  "min_files": 2,
  "cooldown_seconds": 900,
  "enforce": false,
  "ctx_limit": 5,
  "drift_commit_threshold": 20,
  "drift_window_days": 90,
  "proposed_stale_days": 14,
  "journal_retention_days": 90
}
```

---

## 7. CLI surface

Every command takes `--json`. Agents parse machine output; humans read the default. This is not optional garnish, it is what makes the tool usable from a skill without brittle text scraping.

```
dlog init                      scaffold docs/adr, .dlog, hooks config, skill, command
dlog new <title>               [--status --supersedes --depends-on --affects --agent --tags]
dlog list                      [--status --tag --affects]
dlog show <id>
dlog lint                      [--fix]   exit 1 on problems
dlog index                     regenerate docs/adr/README.md (table + mermaid)
dlog ctx <topic>               [--files a,b,c] [--limit N]   relevance-ranked records
dlog journal                   [--session --since --agent --grep] [--export <session>]
dlog audit                     enumerate decision-shaped artifacts with file evidence
dlog drift                     [--since 90d]   stale, orphaned, unresolved
dlog ack                       --none (record a no-decision reply to a nudge) | --drift <id>
dlog hook <event>              session-start | stop | subagent-stop | pre-compact | post-tool-use
dlog prune                     drop journal files past retention
```

`dlog lint --fix` repairs only mechanical defects: missing reciprocal `superseded_by`, status not flipped on a superseded record, filename stem out of sync with id. It never touches prose.

---

## 8. Hook integration

Five events, one binary, wired in `.claude/settings.json`:

| Event | Subcommand | Behaviour | Blocking |
|---|---|---|---|
| `SessionStart` | `hook session-start` | Emits `additionalContext`: proposed ADRs, drift findings if due, current branch. Not the whole log. | no |
| `SubagentStop` | `hook subagent-stop` | Appends journal entry. Runs `async: true`. | no |
| `Stop` | `hook stop` | Appends journal entry, then checks watched paths for changes with no ADR recorded. | configurable |
| `PreCompact` | `hook pre-compact` | Appends a journal entry only. No nudge. | no |
| `PostToolUse` on `docs/adr/**` | `hook post-tool-use` | Lints, regenerates index. Exit 2 hands the defect back to Claude. | no (feedback only) |

The nudge stays on `Stop` rather than moving to `PreCompact`. `PreCompact` exists here purely to bracket context loss: in a long session, decisions can be made and then compacted away before any `Stop` fires, so the journal records changed files and HEAD at the compaction boundary. That preserves a pointer to what was in flight without putting the nudge on an event that fires at an arbitrary moment mid-task, where it would interrupt work rather than review it.

Payload fields on `PreCompact` differ from the Stop family and may not carry a closing assistant message, so this entry is expected to be thinner than the others. Confirm the actual shape against the hooks reference during M2 rather than assuming parity.

The `Stop` check defaults to `additionalContext`, which continues the turn so Claude can act on it, rather than `decision: "block"`. `"enforce": true` switches to blocking, which is capped by `CLAUDE_CODE_STOP_HOOK_BLOCK_CAP` (default 8 consecutive blocks).

Nudge suppression, so the thing stays ignorable-proof by being rare: skip if any `docs/adr/**` file is already dirty; skip if fewer than `min_files` watched files changed; skip if the same changed-file fingerprint was nudged within `cooldown_seconds`. State in `.dlog/state/<worktree>/`, gitignored.

### 8.1 Nudge accounting

A non-blocking nudge that the agent answers with "no design decision here" every time, forever, leaves every other metric green while the tool silently stops working. So the nudge is instrumented:

- Every nudge issued appends an event to `.dlog/state/<worktree>/nudges.jsonl` with the fingerprint and timestamp.
- A nudge counts as *answered* if, within the same session, either an ADR is created or the reply contains an explicit no-decision statement (detected by the next `Stop` hook seeing an ADR file, or by `dlog ack --none`, which the skill instructs the agent to run when declining).
- `dlog drift` reports the rolling 30-day response rate. Below 0.5 is a finding: either the `watch` list is too broad and the nudges are genuinely spurious, or the log has become wallpaper. Both demand a human look at the config, not more automation.

This is deliberately an instrument, not an enforcement. The number exists so the failure is visible, and the failure being visible is the gate for building anything past M2.

Two constraints worth recording, both verified:

- Settings-level hooks fire inside subagents, so journaling and validation need no per-agent wiring.
- Subagents loaded from a *plugin* have their `hooks`, `mcpServers`, and `permissionMode` frontmatter dropped at load time. Since this ships as a plain repo rather than a plugin, that does not bite us, but it forecloses the plugin path later without moving agent files into `.claude/agents/`.

---

## 9. Relevance retrieval

`dlog ctx` answers: which of the accepted decisions should this agent read before touching this code?

```
score(adr) = 3.0 * path_overlap
           + 1.0 * bm25(query, title + tags + decision_section)
           + 0.5 * recency_decay(date, halflife=180d)
           * status_weight
```

- `path_overlap`: fraction of the input file set matching any `affects` glob. Primary signal, and the reason `affects` exists.
- `bm25`: standard lexical scoring over title, tags, and the Decision paragraph. Whether to include the Context section is an open call: excluding it avoids dilution from long prose, but Context is where the domain vocabulary actually lives (idempotency, tombstone, backfill), so exclusion is a hypothesis to test during calibration, not an assertion.
- `status_weight`: accepted 1.0, proposed 0.6, superseded 0.0. Superseded records are excluded from context but reachable via `dlog show`.

Returns id, title, one-line decision, path, and score — and returns *fewer* than `ctx_limit` when scores fall off. Most tasks have zero to two genuinely relevant decisions, so padding to five with weak matches injects confident irrelevance, which is worse than injecting nothing. Cut where score drops below 40% of the top score or below an absolute floor, whichever prunes more.

That shape is also why the calibration metric is **recall of labelled-relevant records**, not precision at 5. Precision at a fixed k is dominated by the padding the cutoff exists to remove; what actually hurts is the agent missing the one decision governing the file it is editing.

Weights live in `.dlog/config.json`, not in the binary. The section 9 numbers are defaults. Thirty labelled pairs is a floor sufficient to reject a broken ranking, not to tune four parameters — treat calibration as "confirm the defaults are not embarrassing," and grow the labelled set from real misses over time.

Called two ways: from `SessionStart` with the diff against the base branch, and on demand from a skill before implementation. Read-only, so it is safe from parallel subagents.

---

## 10. Bootstrap audit

`dlog audit` enumerates decision-shaped artifacts. The binary finds candidates; an agent writes the records. That split keeps the binary deterministic and puts the judgment where judgment belongs.

Scanned:

| Source | Signal |
|---|---|
| `go.mod` | direct requires |
| `package.json` | dependencies |
| `migrations/` | schema shape, per-file |
| `*.tf` | providers, resource types |
| `charts/`, `kustomization.yaml` | topology |
| `Dockerfile`, `compose.yaml` | base images, services |
| `.github/workflows/` | CI topology |
| `Makefile` | build entry points |

Output is JSON: candidate, evidence as `file:line`, and a suggested title. `dlog init` runs it once and hands the list to the `adr` skill for batch authoring.

Deliberately over-inclusive. Rejecting a candidate is cheap; a missed decision stays missed.

---

## 11. Drift detection

Deterministic, from git only. Three findings:

- **stale**: an accepted ADR whose `affects` paths have taken more than `drift_commit_threshold` commits since the ADR's own last modification. The code moved on and the record may not have.
- **orphaned**: `affects` globs matching zero files. Code was moved or deleted.
- **unresolved**: `status: proposed` older than `proposed_stale_days`.

Surfaced at `SessionStart` when due, at most once per day per repo, tracked in `.dlog/state/`. Findings are advisory and never block.

An ADR can carry `drift_ack: 2026-08-04` to silence a stale finding. Precisely: the churn count resets its baseline to the commit at the ack date, so the finding fires again only after a further `drift_commit_threshold` commits land in `affects` paths *after* the ack. Acking is "I looked, the record still holds as of here," not "never tell me again."

---

## 12. Repo layout

```
tools/dlog/
  cmd/dlog/main.go          subcommand dispatch
  internal/adr/             parse, write, lint, index
  internal/journal/         append, query
  internal/rank/            bm25, glob overlap, scoring
  internal/audit/           artifact scanners
  internal/drift/           git churn analysis
  internal/gitx/            common-dir resolution, diff, log
  internal/hook/            payload structs, one file per event
  testdata/                 fixture repos
Makefile                    build to .claude/bin/dlog
.claude/
  settings.json             four hooks
  skills/adr/SKILL.md       when a decision earns a record, how to write one
  commands/adr.md           /adr
  bin/dlog                  gitignored build output
docs/adr/
  README.md                 generated
  <id>.md
.dlog/
  config.json
  journal/<session>.jsonl
  state/                    gitignored
```

Install is `cp -r` plus `make install`. Since this is a binary rather than a script, `make install` must run before the hooks work. When the binary is missing, the hook wrapper does not fail every turn, but it must not fail *silently* either — invisible tracking loss is the worst failure shape, because nothing looks broken while the journal quietly stops. The `SessionStart` hook entry is therefore a one-line shell check that emits a single "dlog binary missing, run make install; decision tracking is off" as `additionalContext` once per session, and all other hook entries exit 0 quietly.

Single-platform (darwin/arm64) is fine for now. A teammate on linux means building on install, which is a Makefile change, not a design change.

---

## 13. Milestones

Effort is not uniform across these, and pretending otherwise is how side projects die in the middle. M1 and M2 are roughly a weekend and have already been prototyped in Python. M3 and M4 are several weekends of unglamorous work: a ranker with a calibration set, eight audit scanners, churn analysis with a false-positive budget. The plan is honest about that by making the cheap 20% — which carries the differentiating value — the committed release, and gating the expensive rest.

### v0.1 — committed

**M1. Core ADR handling**
`new`, `list`, `show`, `lint`, `index`. Frontmatter parser for the YAML subset, with unparseable frontmatter as a lint failure. Shared-root resolution via `--git-common-dir`. Per-worktree state keying. Supersede back-linking with reciprocity enforcement and the dirty-target warning.
*Done when:* five concurrent worktrees each create an ADR and merge cleanly with no conflict and a clean lint.

**M2. Journal and hooks**
`hook` dispatch for all five events. Journal append with `seq`. `journal_refs` inference and the `affects` birth check (sections 5.4 and 6.3). Nudge logic with fingerprint, cooldown, and nudge accounting (section 8.1). `journal` query and `--export`. Missing-binary session warning. `settings.json` wiring.
*Done when:* a session with three parallel subagents produces one journal file containing all three closing summaries; a rejected alternative is recoverable after the subagent contexts are gone; an ADR created at session end has `journal_refs` pointing at the turns that touched its `affects` paths; and `dlog drift` (stub) can report the nudge response rate.

v0.1 ships here and is complete on its own terms. Everything below is optional and individually gated.

### Post-v0.1 — gated

**Gate for all of it:** two weeks of v0.1 in daily use on the target repo, with a nudge response rate above 0.5 and at least 10 real ADRs written. If the response rate is below 0.5, the problem is behavioral and no amount of retrieval or drift machinery fixes it — fix the `watch` config and the skill first. In parallel, run adr-kit on one other repo; if its retrieval and audit hold up, port the v0.1 journal and ids onto it and stop here.

**M3. Retrieval**
`affects` glob matching, BM25, score-cliff cutoff, `ctx` command, `SessionStart` integration. Hand-label at least 30 (task, relevant ADR) pairs from the target repo — a floor, not a target — and confirm the default weights against them.
*Done when:* recall of labelled-relevant records above 0.85, median injected context under 2k tokens, and the cutoff demonstrably returns fewer than `ctx_limit` records on tasks labelled as having one or zero relevant decisions.

**M4. Audit and drift**
All scanners in section 10. Churn analysis with `drift_ack` baseline-reset semantics. `SessionStart` surfacing with daily cadence. Full `drift` report including nudge response rate.
*Done when:* `dlog audit` on the target repo surfaces 15 or more real decisions, and `dlog drift` produces no more than 2 false stale findings against a hand-reviewed baseline.

**M5. Ergonomics and packaging**
`init` including the journal-commit prompt and `.gitignore` management, `prune`, `lint --fix`, skill and command files, pre-commit hook, CI workflow checking lint and index freshness, README.
*Done when:* a fresh repo goes from `cp -r` to a working setup with seeded ADRs in under five minutes.

**M6. Query surface (post-v1)**
`dlog serve`: read-only local HTTP view of the graph, plus the same data over MCP so other agents and tools can query it without shelling out.

Scope deliberately thin. The web view renders the DAG, filters by tag and status, and links to source files. No editing, no auth, bound to localhost. The MCP surface exposes `ctx`, `list`, `show`, and `journal` as read-only tools, which is the same contract the `--json` flags already provide, so the marginal work is transport rather than logic.

Not started until M1 through M5 are in daily use on the target repo. The `--json` output exists precisely so this milestone stays optional: anything the UI would show is already scriptable, and if that turns out to be enough, M6 never happens.

---

## 14. Risks

| Risk | Why it matters | Mitigation |
|---|---|---|
| Filler ADRs | The only real failure mode. An unreadable log is worse than none, and an agent optimising to clear a check will produce exactly this. | Behavioral, not mechanical (section 4): high bar in the skill, empty Alternatives is a warning to humans not a gate for agents, narrow `watch`, non-blocking default, periodic human read of the log |
| Nudge becomes wallpaper | Agent declines every nudge forever while all metrics stay green | Instrumented: response rate in `dlog drift` (section 8.1); below 0.5 blocks the M3+ gate and demands config or skill changes |
| `affects` wrong at birth | Corrupts `journal_refs`, retrieval, and drift at once, with correlated errors | Birth check against the session's actual `changed_files` (section 5.4), plus rot checks in lint and drift |
| Shared mutable state under parallelism | The founding pitch is parallelism; racy state would be self-refuting | Explicit posture per state item (section 5.2): per-worktree keying, idempotent index, dirty-target warning on back-links |
| Journal growth | 5 sessions times many turns per day | Path not content for transcripts, 1200 byte summary cap, `prune` at 90 days, `state/` gitignored |
| Retrieval injects confident irrelevance | Worse than no retrieval | Score-cliff cutoff, recall-based calibration, weights in config; M3 is gated and optional |
| M3–M4 effort sink | Several weekends of a solo side project re-implementing what adr-kit has | Resolved by section 0: v0.1 is the commitment, the rest is gated, and the adr-kit trial runs in parallel with an explicit off-ramp |

## 15. Resolved decisions

| # | Question | Decision |
|---|---|---|
| 1 | Commit the journal? | Per-repo choice. `dlog init` asks, config records `journal_committed`, `.gitignore` follows. `journal --export` covers sharing one session when journals are not committed. |
| 2 | Populate `journal_refs` by hand or infer? | Infer, per section 6.3. Glob overlap first, fall back to entries since the last ADR in the session. Advisory field, `--no-refs` opts out. |
| 3 | Nudge on `Stop` or `PreCompact`? | Keep it on `Stop`. Add `PreCompact` as a journal-only hook so compacted-away reasoning is still bracketed. |
| 4 | `dlog serve` over MCP, and a UI? | Yes, later. M6, post-v1, read-only, localhost, not started until the rest is in daily use. |

## 16. Remaining unknowns

1. `PreCompact` payload shape. Confirm during M2 whether it carries anything equivalent to `last_assistant_message`, or whether that entry is limited to changed files and HEAD.
2. Whether BM25 should include the Context section. Test both during M3 calibration; domain vocabulary lives in Context, dilution lives there too.
3. Journal volume in practice. Five sessions of real work per day is the first honest measurement of whether 90 day retention is generous or tight.
4. Whether 0.5 is the right nudge response-rate threshold, and whether the 15 minute cooldown survives a week of use. Both are config, both start as guesses, both get revisited at the M3 gate with real numbers.
5. Whether `dlog ack --none` actually gets run by the agent when declining a nudge, or whether the skill instruction is ignored under context pressure. If ignored, the response-rate denominator inflates and the metric reads worse than reality — which fails safe, but should be known.
