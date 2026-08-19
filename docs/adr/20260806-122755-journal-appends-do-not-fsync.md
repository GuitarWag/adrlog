---
id: 20260806-122755-journal-appends-do-not-fsync
title: Journal appends do not fsync
status: accepted
date: 2026-08-06
worktree: adrlog
session: 38a4725d-b8f8-49e2-8845-a9d295b20227
affects:
  - internal/journal/**
supersedes: []
superseded_by: []
depends_on: []
journal_refs: []
tags: [journal, performance]
---

## Context

`journal.Append` originally called `f.Sync()` on every write. Measured on this
machine, the Stop hook ran at 63ms mean against a 50ms p99 budget. The
fsync was part of that, alongside five `git` subprocesses at roughly 9ms each.

## Decision

No fsync. The append is one small write under an exclusive `flock`, so another
process reads it immediately; durability past that point buys protection only
against a machine crash.

Combined with collapsing four `git rev-parse` calls into one and caching the
`git status` result within a process, the Stop hook now measures 18.8ms mean and
24.7ms p99 on an idle repo, 38.4ms p99 in the busier scratch repo the milestone
script builds.

## Alternatives considered

Keep the fsync and buy the budget back elsewhere. Rejected: the git subprocess
savings were needed regardless, and spending any of the remaining headroom on
durability for an advisory record is the wrong trade. The journal is explicitly
ephemeral by nature and pruned at 90 days.

Batching or deferring writes to a background process. Rejected as more machinery
than the problem needs, and it would put entries at risk of never being written at
all, which is worse than losing the last one to a kernel panic.

## Consequences

A machine crash can lose journal entries written in the seconds before it. A
process crash cannot, because the page cache survives that. Given the journal records agent
reasoning rather than money or state, that is acceptable.

The 50ms budget is now defended by a check in `scripts/verify-milestones.sh`
rather than by assumption, because it is tight enough that one added subprocess
breaks it.
