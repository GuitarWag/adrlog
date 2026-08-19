---
id: 20260806-122755-decision-records-live-at-the-shared
title: Decision records live at the shared repository root
status: accepted
date: 2026-08-06
worktree: adrlog
session: 38a4725d-b8f8-49e2-8845-a9d295b20227
affects:
  - internal/gitx/**
  - internal/adr/**
supersedes: []
superseded_by: []
depends_on: []
journal_refs: []
tags: [worktrees, storage]
---

## Context

prd §5.2 lists `docs/adr/<id>.md` in the shared-state table, which puts records at
the shared repository root alongside `.adrlog/`. The M1 done-condition in §13 says
five concurrent worktrees "each create an ADR and merge cleanly with no conflict",
and merging only means something if each record is committed on its own branch.
The two readings cannot both hold, and the choice changes where every record is
written.

## Decision

Records resolve against the shared root, the same as `.adrlog/`:
`dirname(git rev-parse --git-common-dir)`. A record written from any of five
worktrees lands in one directory.

The deciding argument is problem 2 in §2: session B contradicting an accepted
decision from session A, because nothing put A's conclusion in front of it. If
records live on branches, B literally cannot see A's record until it merges, and
the founding problem survives the tool built to solve it. Retrieval in M3 keys off
the same set and has the same requirement.

## Alternatives considered

Per-worktree records, committed on the branch that produced them. This is the
better fit for review, because the record arrives in the pull request next to the
code it explains, and it is what "merge cleanly" implies. Rejected because pre-merge
invisibility defeats the point, and because the collision the timestamp id exists
to prevent (§5.1, G1) only arises when sessions share a directory, which is
evidence the shared directory was the intent.

A copy in both places was not seriously considered: two records for one decision
is the drift problem, self-inflicted.

## Consequences

Records for in-flight work appear in the main checkout's working tree, uncommitted,
authored by a session that is not standing there. Whoever works in the main
checkout sees a dirty tree they did not create, and commits records for branches
that have not landed. That is a real cost and the main thing that would make us
revisit.

`scripts/verify-milestones.sh` checks the property directly: five concurrent
worktrees produce five records at the shared root, none scattered into a worktree,
and removing a worktree does not take a record with it.
