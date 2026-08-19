---
id: 20260806-122755-the-nudge-response-rate-finding-follows
title: The nudge response-rate finding follows the gate not the prose
status: accepted
date: 2026-08-06
worktree: adrlog
session: 38a4725d-b8f8-49e2-8845-a9d295b20227
affects:
  - internal/state/**
supersedes: []
superseded_by: []
depends_on: []
journal_refs: []
tags: [nudge, metrics]
---

## Context

The plan stated the response-rate threshold twice, in incompatible forms. The
goal and the gate both want a rate "above 0.5". The passage describing the
instrument worded the finding as "below 0.5 is a finding".

A rate of exactly 0.5 therefore fails the gate while reporting nothing. That is
the one reading that makes the instrument useless. The number exists so the
failure is visible, and a silent gate failure is the failure it was built to
catch.

## Decision

The finding fires whenever the rate is not above the floor, meaning `rate <= 0.5`,
so the instrument and the gate it feeds cannot disagree. `ResponseFloor` lives in
`internal/state/state.go`, not in `.adrlog/config.json`, because changing it changes
what the M3 gate means and that should not be a per-repo setting.

## Alternatives considered

Follow the instrument's wording literally with a strict `<`. Rejected, because it
creates a value that fails the gate silently.

Relax the gate to "0.5 or above" instead, which would make the other wording
correct. Rejected because the gate wording appeared twice against the other's
once, and because the threshold is an admitted guess. When in doubt, take the
reading that reports.

## Consequences

At exactly 0.5 the tool reports a finding and blocks the M3 gate. Whether 0.5 is
the right number at all is still open and gets revisited with real usage
data, at which point this record is the thing to revise.
