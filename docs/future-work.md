# Future work

Designed, not built. Each piece below waits on the same gate, and the gate is
behavioural rather than technical.

## The gate

Two weeks of daily use, a prompt response rate above 0.5, and at least 10 real
records written.

If the response rate sits below 0.5, the problem is behavioural and none of the
machinery here fixes it. Fix the `watch` list and the authoring skill first. A
retrieval ranker that surfaces records nobody writes is elaborate nothing.

Run `adrlog drift` to check where the rate stands.

There is also an off-ramp worth taking seriously. `rvdbreemen/adr-kit` already
does retrieval and a bootstrap audit. Running it on one repo in parallel is
cheaper than building M3 and M4, and if it holds up, the right move is to port
this journal and its ids onto that and stop.

## Retrieval

`adrlog ctx <topic>` would answer one question: which accepted decisions should
an agent read before touching this code?

```
score(record) = 3.0 * path_overlap
              + 1.0 * bm25(query, title + tags + decision_section)
              + 0.5 * recency_decay(date, halflife=180d)
              * status_weight
```

`path_overlap` is the fraction of the input file set matching any `affects`
glob. It is the primary signal and the reason `affects` exists at all: matching
paths beats text similarity, and it is deterministic.

`bm25` scores title, tags and the Decision paragraph. Whether to include Context
is an open question. Excluding it avoids dilution from long prose, but Context
is where the domain vocabulary lives. Test both during calibration rather than
guessing.

`status_weight` is 1.0 accepted, 0.6 proposed, 0.0 superseded. Superseded
records stay reachable through `adrlog show`, they just never get injected.

The cutoff matters more than the ranking. Return **fewer** than `ctx_limit` when
scores fall off, cutting below 40% of the top score or an absolute floor,
whichever prunes more. Most tasks have zero to two genuinely relevant decisions.
Padding to five with weak matches injects confident irrelevance, which is worse
than injecting nothing.

That is also why the calibration metric is recall of labelled-relevant records
rather than precision at 5. Precision at a fixed k mostly measures the padding
the cutoff exists to remove. What actually hurts is the agent missing the one
decision governing the file it is editing.

Weights belong in `.adrlog/config.json`, not the binary. Thirty labelled pairs
is enough to reject a broken ranking, not to tune four parameters. Treat
calibration as confirming the defaults are not embarrassing, then grow the
labelled set from real misses.

**Done when:** recall above 0.85, median injected context under 2k tokens, and
the cutoff demonstrably returns fewer than `ctx_limit` records on tasks with one
or zero relevant decisions.

## Bootstrap audit

`adrlog audit` would list decision-shaped artifacts already in a codebase. A log
seeded from real decisions beats one that starts empty, because the decisions
already in force are the ones agents violate.

The binary finds candidates and an agent writes the records. That split keeps
the binary deterministic and puts judgement where judgement belongs.

| Source | Signal |
|---|---|
| `go.mod` | direct requires |
| `package.json` | dependencies |
| `migrations/` | schema shape, per file |
| `*.tf` | providers, resource types |
| `charts/`, `kustomization.yaml` | topology |
| `Dockerfile`, `compose.yaml` | base images, services |
| `.github/workflows/` | CI topology |
| `Makefile` | build entry points |

Output is JSON: the candidate, evidence as `file:line`, and a suggested title.
Deliberately over-inclusive, because rejecting a candidate is cheap and a missed
decision stays missed.

**Done when:** a real codebase yields 15 or more genuine decisions.

## Drift detection

Deterministic, from git alone. Three findings:

**stale.** An accepted record whose `affects` paths have taken more than
`drift_commit_threshold` commits since the record itself last changed. The code
moved on and the record may not have.

**orphaned.** `affects` globs matching zero files. The code was moved or deleted.
`adrlog lint` already warns about this.

**unresolved.** A record still `proposed` after `proposed_stale_days`.

Findings are advisory and never block. `SessionStart` would surface them at most
once a day.

A record can carry `drift_ack: 2026-08-04` to silence a stale finding. The churn
count then resets its baseline to the commit at that date, so the finding fires
again only after a further `drift_commit_threshold` commits land in `affects`
paths after the ack. Acking means "I looked, this still holds as of here", not
"stop telling me".

**Done when:** no more than 2 false stale findings against a hand-reviewed
baseline.

## Ergonomics

`adrlog init` to scaffold a repository, ask whether to commit the journal, and
manage `.gitignore`. `adrlog prune` to drop journals past
`journal_retention_days`. `adrlog lint --fix` for mechanical defects only:
a missing reciprocal `superseded_by`, a status not flipped on a superseded
record, a filename out of step with its id. Never prose.

Plus the authoring skill and slash command, and a pre-commit hook.

**Done when:** a fresh repository reaches a working setup with seeded records in
under five minutes.

## Query surface

`adrlog serve`: a read-only local view of the graph, plus the same data over MCP
so other tools can query it without shelling out.

Deliberately thin. Render the graph, filter by tag and status, link to source
files. No editing, no auth, bound to localhost. The MCP side exposes `ctx`,
`list`, `show` and `journal` as read-only tools, which is the contract `--json`
already provides, so the work is transport rather than logic.

Not started until everything above is in daily use. The `--json` output exists
precisely so this stays optional. If scripting turns out to be enough, this
never happens.

## Open questions

1. Whether BM25 should include the Context section. Domain vocabulary lives
   there, and so does dilution.
2. Journal volume in practice. Five sessions of real work per day is the first
   honest measurement of whether 90-day retention is generous or tight.
3. Whether 0.5 is the right response-rate threshold, and whether the 15-minute
   cooldown survives a week of use. Both started as guesses.
4. Whether an agent under context pressure actually runs `adrlog ack --none`
   when it declines. If it does not, the denominator inflates and the rate reads
   worse than reality. That fails safe, but it should be known.
