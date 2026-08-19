---
id: 20260819-141610-support-macos-and-linux-only-and
title: Support macOS and Linux only, and exclude Windows at build time
status: accepted
date: 2026-08-19
branch: main
worktree: dlog
session: 38a4725d-b8f8-49e2-8845-a9d295b20227
affects:
  - internal/flock/**
  - .github/workflows/**
supersedes: []
superseded_by: []
depends_on: []
journal_refs: [38a4725d-b8f8-49e2-8845-a9d295b20227#26, 38a4725d-b8f8-49e2-8845-a9d295b20227#25, 38a4725d-b8f8-49e2-8845-a9d295b20227#24, 38a4725d-b8f8-49e2-8845-a9d295b20227#23, 38a4725d-b8f8-49e2-8845-a9d295b20227#22, 38a4725d-b8f8-49e2-8845-a9d295b20227#21, 38a4725d-b8f8-49e2-8845-a9d295b20227#20, 38a4725d-b8f8-49e2-8845-a9d295b20227#19, 38a4725d-b8f8-49e2-8845-a9d295b20227#18, 38a4725d-b8f8-49e2-8845-a9d295b20227#17]
tags: [platform, build]
---

## Context

`adrlog` assigns a journal `seq` by reading the current high water mark and then
appending. Three subagents finishing at once in one session hit that path
together, so the read and the write are held under an exclusive `flock`. Without
it every concurrent caller receives the same number, and `journal_refs` points at
`seq` to name one specific turn.

`syscall.Flock` does not exist on Windows. Before the first release, a Windows
build failed with `undefined: syscall.Flock`, which reads like an oversight
rather than a decision.

## Decision

macOS and Linux, on amd64 and arm64. The lock lives in `internal/flock` behind
`//go:build unix`, so a Windows build stops with `build constraints exclude all
Go files` and the package comment states why.

CI runs the whole suite on both platforms rather than one, and cross-compiles all
four targets, so the claim is checked rather than asserted.

## Alternatives considered

Implement the lock on Windows with `syscall.LockFileEx`, which is in the standard
library and would need roughly twenty lines. Rejected for now, because it cannot
be tested here and this is the lock that keeps the journal honest. A wrong lock
does not crash; it hands two turns the same `seq`, quietly, and the corruption
surfaces later as a `journal_refs` pointer that names two different turns. That
is the exact class of defect this tool is built to prevent, so it is the worst
possible place to ship untested code. The port belongs in `flock_windows.go`
alongside a CI job that runs the concurrent path.

Drop file locking and use an `O_EXCL` lockfile instead, which is portable.
Rejected because it replaces a kernel-managed lock with one that leaks whenever a
process dies holding it, and hook processes are killed routinely.

Leave the compile error as it was. Rejected: `undefined: syscall.Flock` tells a
stranger nothing about whether Windows is unsupported or merely broken.

## Consequences

Windows users cannot install the tool, and `go install` fails for them at build
time rather than at runtime. That is the intended outcome, and the README states
the supported platforms rather than leaving them to discover it.

Anyone adding a second lock call must route it through `internal/flock` rather
than calling `syscall` directly, or the constraint quietly stops holding.

What would make us revisit: somebody who runs Claude Code on Windows wanting
this, and being in a position to test the concurrent path there.
