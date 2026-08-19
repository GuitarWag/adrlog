//go:build unix

// Package flock takes an advisory exclusive lock on an open file.
//
// Two callers need it, and both need it for the same reason: a read-then-append
// that must not interleave. The journal assigns seq by reading the current high
// water mark, and journal_refs points at seq to name one specific turn,
// so two subagents finishing together must not be handed the same number. The
// nudge ledger checks the cooldown and then writes, and two Stop hooks racing
// there produced two prompts for one change.
//
// Unix only, deliberately. Windows has LockFileEx, but shipping an untested
// implementation of the thing that guarantees seq uniqueness is worse than not
// building there at all — a wrong lock fails silently and corrupts the record
// the tool exists to keep. A Windows port belongs in flock_windows.go, with a
// CI job that actually exercises the concurrent path.
package flock

import (
	"os"
	"syscall"
)

// Lock blocks until it holds an exclusive lock on f, and returns the release.
// The release is safe to defer and never returns an error worth acting on.
func Lock(f *os.File) (unlock func(), err error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return func() {}, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
