#!/usr/bin/env bash
# Checks the M1 and M2 done-conditions from prd §13.
#
# These are cross-process and cross-worktree by nature — five sessions racing for
# an id, three subagent hooks appending to one file — so they cannot live in
# `go test`. Everything runs in a scratch repo under $TMPDIR; nothing touches the
# checkout you are standing in.
set -euo pipefail

DLOG="$(cd "$(dirname "$0")/.." && pwd)/.claude/bin/dlog"
[ -x "$DLOG" ] || { echo "build first: make install"; exit 1; }

SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT
MAIN="$SCRATCH/main"

pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }
FAILED=0

mkdir -p "$MAIN"/{internal/ledger,cmd/dlog}
cd "$MAIN"
git init -q -b main
git config user.email t@example.com
git config user.name test
echo package ledger > internal/ledger/store.go
echo package main > cmd/dlog/main.go
git add -A && git commit -qm init

echo
echo "M1 — core ADR handling"

# Five concurrent worktrees each create a record. A sequential counter is what
# collides here; timestamp ids need no coordination (prd §5.1, G1).
for i in 1 2 3 4 5; do
  git worktree add -q -b "feat/w$i" "$SCRATCH/w$i" main
done
for i in 1 2 3 4 5; do
  ( cd "$SCRATCH/w$i" && "$DLOG" new "Decision from worktree $i" \
      --status accepted --affects "internal/ledger/**" --tags concurrent >/dev/null 2>&1 ) &
done
wait

COUNT=$(find "$MAIN/docs/adr" -name '*.md' ! -name README.md | wc -l | tr -d ' ')
[ "$COUNT" = 5 ] && pass "5 concurrent worktrees produced 5 records, no lost writes" \
                 || fail "expected 5 records at the shared root, found $COUNT"

# The shared root is the point: a worktree writing to its own toplevel would
# scatter these and lose them when the worktree is removed (prd §5.2).
STRAY=$(find "$SCRATCH"/w[1-5] -name '*.md' -path '*docs/adr*' 2>/dev/null | wc -l | tr -d ' ')
[ "$STRAY" = 0 ] && pass "no records scattered into individual worktrees" \
                 || fail "$STRAY record(s) written to a worktree instead of the shared root"

UNIQUE=$(find "$MAIN/docs/adr" -name '*.md' ! -name README.md -exec basename {} \; | sort -u | wc -l | tr -d ' ')
[ "$UNIQUE" = "$COUNT" ] && pass "all ids unique" || fail "id collision: $UNIQUE unique of $COUNT"

if "$DLOG" lint >/dev/null 2>&1; then pass "lint clean across all five"; else
  fail "lint failed:"; "$DLOG" lint || true
fi

# Removing a worktree must not take the decision with it.
git worktree remove --force "$SCRATCH/w5"
AFTER=$(find "$MAIN/docs/adr" -name '*.md' ! -name README.md | wc -l | tr -d ' ')
[ "$AFTER" = 5 ] && pass "records survive worktree removal" || fail "lost a record with the worktree"

# Supersede: reciprocity in both directions, and the target's status flipped.
OLD=$("$DLOG" list --json | grep '"id"' | head -1 | sed 's/.*: "//;s/".*//')
"$DLOG" new "Replaces the first decision" --status accepted --supersedes "$OLD" \
    --affects "internal/ledger/**" >/dev/null 2>&1
if grep -q 'status: superseded' "docs/adr/$OLD.md" && grep -q 'superseded_by: \[' "docs/adr/$OLD.md"; then
  pass "supersede back-link written and status flipped"
else
  fail "supersede back-link missing in $OLD.md"
fi
"$DLOG" lint >/dev/null 2>&1 && pass "reciprocity lints clean" || fail "reciprocity broken after supersede"

# Break the reciprocity by hand: lint has to notice, or a lost write stays silent.
cp "docs/adr/$OLD.md" "$SCRATCH/backlink-intact"
python3 - "docs/adr/$OLD.md" <<'PY'
import re,sys
p=sys.argv[1]; s=open(p).read()
open(p,'w').write(re.sub(r'superseded_by: \[[^\]]*\]','superseded_by: []',s))
PY
"$DLOG" lint >/dev/null 2>&1 && fail "lint passed a broken back-link" \
                             || pass "lint catches a lost back-link"
# Put it back: everything after this point expects a consistent tree.
cp "$SCRATCH/backlink-intact" "docs/adr/$OLD.md"

# The index is generated from the full set, so concurrent regeneration is safe
# only if it is byte-identical regardless of who runs it last (prd §5.2).
"$DLOG" index >/dev/null; cp docs/adr/README.md "$SCRATCH/idx1"
"$DLOG" index >/dev/null; cp docs/adr/README.md "$SCRATCH/idx2"
cmp -s "$SCRATCH/idx1" "$SCRATCH/idx2" && pass "index regeneration is deterministic" \
                                       || fail "index differs between runs"

# An unreadable record is a defect, never an absence (prd §6.1). Capture the
# output rather than piping: lint exits 1 here by design, and pipefail would
# report that as the check itself failing.
printf -- '---\nid: broken\ntitle: X\nmeta: {flow: mapping}\n---\n' > docs/adr/broken.md
LINTOUT="$("$DLOG" lint 2>&1 || true)"
case "$LINTOUT" in
  *"unreadable record"*) pass "unparseable frontmatter is a lint failure" ;;
  *) fail "unparseable record was skipped silently" ;;
esac
rm docs/adr/broken.md

echo
echo "M2 — journal and hooks"

SESSION=sess-$$
hook() { echo "$2" | "$DLOG" hook "$1"; }

# Three subagents finishing at once, in one session, all appending to one file.
for a in schema-reviewer qa-adversary perf-checker; do
  ( hook subagent-stop "{\"session_id\":\"$SESSION\",\"cwd\":\"$MAIN\",\"agent_type\":\"$a\",\"transcript_path\":\"/tmp/$a.jsonl\",\"last_assistant_message\":\"$a rejected the hard-delete approach; it breaks the immutable ledger requirement.\"}" >/dev/null 2>&1 ) &
done
wait

JOURNALS=$(find "$MAIN/.dlog/journal" -name '*.jsonl' | wc -l | tr -d ' ')
[ "$JOURNALS" = 1 ] && pass "one journal file for the session" || fail "expected 1 journal file, got $JOURNALS"

for a in schema-reviewer qa-adversary perf-checker; do
  "$DLOG" journal --session "$SESSION" --json | grep -q "$a" \
    && pass "closing summary captured: $a" || fail "missing summary for $a"
done

SEQS=$("$DLOG" journal --session "$SESSION" --json | grep -c '"seq"')
USEQ=$("$DLOG" journal --session "$SESSION" --json | grep '"seq"' | sort -u | wc -l | tr -d ' ')
[ "$SEQS" = "$USEQ" ] && pass "seq unique under concurrent append" || fail "duplicate seq: $SEQS entries, $USEQ distinct"

# G2: the reasoning survives the contexts being discarded.
"$DLOG" journal --grep "hard-delete" --json | grep -q immutable \
  && pass "rejected alternative recoverable after contexts are gone" \
  || fail "rejected alternative not recoverable"

# journal_refs must point at the turns that touched the affects paths (prd §6.3).
hook stop "{\"session_id\":\"$SESSION\",\"cwd\":\"$MAIN\",\"last_assistant_message\":\"wrapping up\"}" >/dev/null 2>&1 || true
REFS=$(cd "$MAIN" && CLAUDE_CODE_SESSION_ID="$SESSION" "$DLOG" new "Tombstone column over hard delete" \
        --status accepted --affects "internal/ledger/**" --json 2>/dev/null | grep -A5 journal_refs)
echo "$REFS" | grep -q "$SESSION#" && pass "journal_refs inferred from the session's turns" \
                                   || fail "journal_refs empty: $REFS"

# G7: silence when there is nothing to say.
OUT=$(hook stop "{\"session_id\":\"quiet-$$\",\"cwd\":\"$MAIN\"}" 2>/dev/null || true)
[ -z "$OUT" ] && pass "hook is silent with no watched changes" || fail "hook spoke unprompted: $OUT"

# Commit what exists first. An uncommitted record under docs/adr/ suppresses the
# nudge by design — the session is already writing one, so asking is noise
# (prd §8) — and that rule would otherwise mask the whole test.
git add -A && git commit -qm records

# The nudge: enough watched files change, nothing recorded, so it asks once.
echo "// change" >> internal/ledger/store.go
echo "// change" >> cmd/dlog/main.go
NUDGE=$(hook stop "{\"session_id\":\"nudge-$$\",\"cwd\":\"$MAIN\",\"last_assistant_message\":\"done\"}" 2>/dev/null || true)
echo "$NUDGE" | grep -q additionalContext && pass "nudge fires on watched changes" || fail "no nudge: $NUDGE"

# ...and stays quiet inside the cooldown for the same file set (prd §8).
AGAIN=$(hook stop "{\"session_id\":\"nudge-$$\",\"cwd\":\"$MAIN\",\"last_assistant_message\":\"done\"}" 2>/dev/null || true)
[ -z "$AGAIN" ] && pass "cooldown suppresses a repeat nudge" || fail "nudged twice for one file set"

# G7: p99 under 50ms. Measured at scale, not on the handful of files the checks
# above created — the defects this catches (a per-glob scan of every tracked file,
# a per-append rescan of the whole journal) are invisible on a small repo and put
# the hook 3x over budget on a real one.
python3 - <<'PY'
import json,os
os.makedirs("docs/adr",exist_ok=True); os.makedirs(".dlog/journal",exist_ok=True)
for i in range(600):
    d="internal/scale%d"%(i%30); os.makedirs(d,exist_ok=True); open("%s/f%d.go"%(d,i),"w").write("package p\n")
for i in range(100):
    rid="20260801-%06d-scale-record-%d"%(i,i)
    open("docs/adr/%s.md"%rid,"w").write(
      "---\nid: %s\ntitle: Scale record %d\nstatus: accepted\ndate: 2026-08-01\n"
      "affects:\n  - internal/scale%d/**\n  - internal/scale%d/**\n---\n\n"
      "## Context\nc\n## Decision\nd\n## Alternatives considered\na\n## Consequences\nq\n"
      % (rid,i,i%30,(i+1)%30))
with open(".dlog/journal/scale.jsonl","w") as f:
    for i in range(1,5001):
        f.write(json.dumps({"seq":i,"ts":"2026-08-06T10:00:00Z","event":"Stop",
                            "session":"scale","summary":"x"*200})+"\n")
PY
git add -A && git commit -qm scale

python3 - "$DLOG" "$MAIN" <<'PY'
import subprocess,sys,time,json,glob
d,cwd=sys.argv[1],sys.argv[2]
some_adr=sorted(glob.glob(cwd+"/docs/adr/*.md"))[0]
bad=0
# 100 samples so index 98 is genuinely the 99th percentile. At 60 samples the old
# arithmetic picked the 58th value, which is p98 reported as p99.
for name,args,extra in [
    ("stop",          ["hook","stop"],          {}),
    ("session-start", ["hook","session-start"], {}),
    ("post-tool-use", ["hook","post-tool-use"], {"tool_input":{"file_path":some_adr}}),
]:
    p=json.dumps({"session_id":"latency","cwd":cwd,"last_assistant_message":"x",**extra}).encode()
    for _ in range(5): subprocess.run([d]+args,input=p,capture_output=True,cwd=cwd)
    ts=[]
    for _ in range(100):
        s=time.time(); subprocess.run([d]+args,input=p,capture_output=True,cwd=cwd); ts.append((time.time()-s)*1000)
    ts.sort(); p99=ts[98]
    ok = p99 < 50
    bad += 0 if ok else 1
    print("  %s %s hook p99 %.1f ms (budget 50)" % ("ok  " if ok else "FAIL", name, p99))
sys.exit(1 if bad else 0)
PY
[ $? = 0 ] || FAILED=1

CLAUDE_CODE_SESSION_ID="nudge-$$" "$DLOG" ack --none >/dev/null
"$DLOG" drift --json | grep -q '"rate"' && pass "drift reports the nudge response rate" \
                                        || fail "drift did not report a rate"
RATE=$("$DLOG" drift --json | grep '"rate"' | head -1)
pass "response rate: $(echo "$RATE" | tr -d ' ,')"

echo
echo "regressions — defects found by adversarial review"

# S1: an ADR from one session must not answer another session's nudge. Records are
# shared across worktrees, so mtime-matching let any record anywhere close every
# open nudge and pin the M3 gate metric near 1.0 (prd §8.1 says "within the same
# session").
cd "$SCRATCH/w2"
echo "// a" >> internal/ledger/store.go
echo "// b" >> cmd/dlog/main.go
git -C "$SCRATCH/w2" add -A >/dev/null 2>&1 || true
hook stop "{\"session_id\":\"sessB\",\"cwd\":\"$SCRATCH/w2\",\"last_assistant_message\":\"x\"}" >/dev/null 2>&1 || true
LEDGER="$MAIN/.dlog/state/w2/nudges.jsonl"
if [ -s "$LEDGER" ]; then pass "nudge recorded in worktree w2"; else fail "no nudge in w2"; fi

# A different session, in a different worktree, writes an unrelated record.
( cd "$SCRATCH/w1" && CLAUDE_CODE_SESSION_ID=sessA "$DLOG" new "Unrelated decision from another session" \
    --status accepted --affects "internal/ledger/**" >/dev/null 2>&1 )
sleep 1
hook stop "{\"session_id\":\"sessB\",\"cwd\":\"$SCRATCH/w2\",\"last_assistant_message\":\"y\"}" >/dev/null 2>&1 || true
if grep -q '"kind":"ack"' "$LEDGER"; then
  fail "another session's record answered sessB's nudge"
else
  pass "another session's record does not answer the nudge"
fi

# Touching every record must not answer it either.
touch "$MAIN"/docs/adr/*.md
hook stop "{\"session_id\":\"sessB\",\"cwd\":\"$SCRATCH/w2\",\"last_assistant_message\":\"z\"}" >/dev/null 2>&1 || true
grep -q '"kind":"ack"' "$LEDGER" && fail "touching records answered the nudge" \
                                 || pass "touching records does not answer the nudge"

# ...but this session writing one does.
( cd "$SCRATCH/w2" && CLAUDE_CODE_SESSION_ID=sessB "$DLOG" new "Decision that answers the nudge" \
    --status accepted --affects "internal/ledger/**" >/dev/null 2>&1 )
grep -q '"kind":"ack"' "$LEDGER" && pass "this session's record answers its own nudge" \
                                 || fail "sessB's own record did not answer its nudge"
cd "$MAIN"

# S2: an index missing records nobody was told about is the silent skip §6.1 rules
# out. Its output is stable across runs, so a CI freshness check would pass too.
cp docs/adr/README.md "$SCRATCH/idx-before"
printf -- '---\nid: hidden\ntitle: Secret\nmeta: {flow: mapping}\n---\n' > docs/adr/hidden.md
if "$DLOG" index >/dev/null 2>&1; then fail "index published while a record was unreadable"; else
  cmp -s docs/adr/README.md "$SCRATCH/idx-before" && pass "index refuses to publish an incomplete set" \
                                                  || fail "index refused but wrote anyway"
fi
# Capture, don't pipe: index exits 1 here by design and pipefail would read that
# as the check failing.
IDXJSON="$("$DLOG" index --json 2>/dev/null || true)"
case "$IDXJSON" in
  *'"written": false'*) pass "index --json reports the refusal" ;;
  *) fail "index --json hid the refusal: $IDXJSON" ;;
esac
rm docs/adr/hidden.md

# S5: a bogus target must not leave an earlier target already mutated, superseded,
# and pointing at a record that will never exist.
KEEP=$("$DLOG" list --status accepted --json | grep '"id"' | head -1 | sed 's/.*: "//;s/".*//')
cp "docs/adr/$KEEP.md" "$SCRATCH/keep-before"
"$DLOG" new "Should not be created" --supersedes "$KEEP" --supersedes does-not-exist >/dev/null 2>&1 \
  && fail "new succeeded with a bogus --supersedes" || true
cmp -s "docs/adr/$KEEP.md" "$SCRATCH/keep-before" && pass "a failed --supersedes leaves targets untouched" \
                                                  || fail "target was mutated before the command failed"
[ -z "$(find docs/adr -name '*should-not-be-created*')" ] && pass "no orphan record written" \
                                                          || fail "record written despite the failure"

# S3: a newline in a title injected frontmatter fields and dropped the rest of the
# title, producing a record that lints clean and describes something else.
"$DLOG" new "$(printf 'Harmless title\nauthor:injected\ndrift_ack:2099-01-01')" >/dev/null 2>&1 \
  && fail "a title with a newline was accepted" || pass "a title with a newline is rejected"

"$DLOG" lint >/dev/null 2>&1 && pass "lint still clean after the regression suite" \
                             || { fail "lint dirty at the end:"; "$DLOG" lint || true; }

echo
if [ "$FAILED" = 0 ]; then echo "M1 and M2 done-conditions hold."; else echo "FAILURES above."; fi
exit $FAILED
