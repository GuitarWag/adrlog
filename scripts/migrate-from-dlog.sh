#!/usr/bin/env bash
# Moves a repository from the tool's former name to the current one.
#
# The rename changed the opt-in marker from .dlog/ to .adrlog/, so a repository
# holding the old directory stopped being tracked. Nothing else changed: journal
# lines, record frontmatter and config keys are identical, so the directory
# carries over as it stands and docs/adr/ needs no edit at all.
#
# Usage: scripts/migrate-from-dlog.sh [repo ...]     (default: the current repo)
#
# Idempotent. It stages a `git mv` but never commits, so the rename stays
# separate from whatever work is already in the tree.
set -euo pipefail

migrate() {
  local repo="$1"
  cd "$repo" || { echo "$repo: cannot enter"; return 1; }

  # The shared root, the same one the tool resolves: from inside a worktree this
  # is the main checkout, which is where .dlog/ actually lives.
  local common
  common=$(git rev-parse --git-common-dir 2>/dev/null) || { echo "$repo: not a git repository"; return 1; }
  case "$common" in /*) ;; *) common="$PWD/$common" ;; esac
  cd "$(dirname "$common")"

  echo "$PWD"

  if [ -d .adrlog ] && [ -d .dlog ]; then
    echo "  both .dlog/ and .adrlog/ exist — merge them by hand, refusing to guess"
    return 1
  fi
  if [ ! -d .dlog ]; then
    echo "  nothing to do"
    return 0
  fi

  # git mv keeps the file history when the directory is tracked. A journal that
  # was never committed is just files, so plain mv covers it.
  if git ls-files --error-unmatch .dlog >/dev/null 2>&1; then
    git mv .dlog .adrlog
    echo "  git mv .dlog .adrlog          (staged, not committed)"
  else
    mv .dlog .adrlog
    echo "  mv .dlog .adrlog              (untracked)"
  fi

  if [ -f .gitignore ] && grep -q '\.dlog' .gitignore; then
    sd -F '.dlog' '.adrlog' .gitignore
    echo "  updated .gitignore"
  fi

  # Leftovers from the per-repo install that predated the global one. A repo-local
  # hook would now double every journal entry, because global hooks run too.
  if [ -e .claude/bin/dlog ]; then
    rm -f .claude/bin/dlog
    echo "  removed .claude/bin/dlog"
  fi
  if [ -f .claude/settings.json ] && grep -q 'dlog' .claude/settings.json; then
    echo "  WARNING: .claude/settings.json still has dlog hooks — delete them,"
    echo "           the global hooks already cover this repository"
  fi

  echo "  done"
}

if [ $# -eq 0 ]; then
  migrate "$PWD"
else
  for r in "$@"; do (migrate "$r"); done
fi
