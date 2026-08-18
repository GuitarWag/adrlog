---
id: 20260810-124629-install-one-global-binary-with-per
title: Install one global binary with per-repo opt-in
status: accepted
date: 2026-08-10
branch: main
worktree: adrlog
session: 38a4725d-b8f8-49e2-8845-a9d295b20227
affects:
  - internal/hook/**
  - Makefile
supersedes: []
superseded_by: []
depends_on: []
journal_refs: []
tags: [install, hooks]
---

## Context

prd §12 describes install as `cp -r` plus `make install` into each repository:
the binary at `.claude/bin/adrlog`, hooks in that repo's `.claude/settings.json`.
That is one wiring step per repository, and the binary duplicated into every one
of them.

The requirement that broke it was wanting adrlog available in any repository
without repeating the setup.

## Decision

One binary at `~/.local/bin/adrlog`, hooks declared once in
`~/.claude/settings.json`, and a repository opts in by having a `.adrlog/`
directory at its shared root. `hook.OptedIn` checks for it and every event
returns immediately when it is absent.

The marker is a directory, not a config file, because config is optional — every
field in §6.4 has a default — so requiring one would mean inventing an empty file
to act as a switch. `adrlog new` creates `.adrlog/`, so writing a first record turns a
repository on without a separate step.

The alternative to an opt-in marker was global hooks that run everywhere. That
applies the default `watch` list to repositories nobody chose to track, and §4 and
§8.1 both say spurious nudges are how the log becomes wallpaper and the response
rate stops meaning anything. Tracking everything by default would corrupt the one
metric that gates M3 onward.

## Alternatives considered

Per-repo install as written in §12. Rejected for the repetition, and because five
copies of a binary drift the moment one repository is rebuilt and another is not.

Global hooks with no opt-in. Rejected above: it trades a setup step for a corrupted
gate metric.

A config file as the marker rather than a directory. Rejected because it makes an
optional file mandatory, and an empty config that exists only to be present is the
kind of thing someone later deletes as dead weight.

## Consequences

Silence in an un-opted repository is a deliberate exception to §12's rule that
tracking must never fail silently. The distinction being drawn is that an un-opted
repository is not tracking-lost, it is tracking-never-asked-for. Where the rule
still applies it is still enforced: the SessionStart hook checks for
`$CLAUDE_PROJECT_DIR/.adrlog` and warns about a missing binary only in a repository
that did opt in.

This repository's own `.claude/settings.json` is deleted. Keeping it would have
double-fired every hook here, since project and global hooks both run — two
journal entries and two nudges per turn.

`make install-global` builds to `~/.local/bin`. Upgrading is that one command;
nothing needs touching per repository afterwards.
