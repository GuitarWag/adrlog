package state

import (
	"sync"
	"testing"
	"time"
)

func at(t time.Time, d time.Duration) string { return t.Add(d).UTC().Format(time.RFC3339) }

// The response rate is the instrument that makes "the nudge became wallpaper"
// visible (prd §8.1), and it gates every milestone past M2 (prd §13). A rate that
// over-counts would hide exactly the failure it exists to expose.
func TestResponseRate(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{TS: at(now, -5*time.Hour), Kind: KindNudge, Session: "a", Fingerprint: "f1"},
		{TS: at(now, -4*time.Hour), Kind: KindAck, Session: "a", How: AckADR},
		{TS: at(now, -3*time.Hour), Kind: KindNudge, Session: "b", Fingerprint: "f2"},
		{TS: at(now, -2*time.Hour), Kind: KindNudge, Session: "c", Fingerprint: "f3"},
		{TS: at(now, -1*time.Hour), Kind: KindAck, Session: "c", How: AckNone},
	}
	r := ResponseRate(events, now)
	if r.Nudges != 3 || r.Answered != 2 {
		t.Fatalf("got %d/%d, want 2/3", r.Answered, r.Nudges)
	}
	if r.Rate < 0.66 || r.Rate > 0.67 {
		t.Errorf("rate = %v", r.Rate)
	}
	if r.Finding != "" {
		t.Errorf("0.67 is above the floor, should not be a finding: %q", r.Finding)
	}
}

// One decline must not clear a backlog. If it did, an agent declining once per
// session while ignoring every later nudge would read as fully responsive.
func TestOneAckAnswersOneNudge(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{TS: at(now, -5*time.Hour), Kind: KindNudge, Session: "a"},
		{TS: at(now, -4*time.Hour), Kind: KindNudge, Session: "a"},
		{TS: at(now, -3*time.Hour), Kind: KindAck, Session: "a", How: AckNone},
	}
	r := ResponseRate(events, now)
	if r.Answered != 1 || r.Nudges != 2 {
		t.Fatalf("got %d/%d, want 1/2", r.Answered, r.Nudges)
	}
	if r.Finding == "" {
		t.Error("0.5 is not above the floor; expected a finding")
	}
}

// An ack cannot answer a nudge that had not happened yet.
func TestAckBeforeNudgeDoesNotCount(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{TS: at(now, -5*time.Hour), Kind: KindAck, Session: "a", How: AckNone},
		{TS: at(now, -4*time.Hour), Kind: KindNudge, Session: "a"},
	}
	if r := ResponseRate(events, now); r.Answered != 0 {
		t.Errorf("answered = %d, want 0", r.Answered)
	}
}

// An ack in one session must not answer another session's nudge.
func TestAckIsPerSession(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{TS: at(now, -5*time.Hour), Kind: KindNudge, Session: "a"},
		{TS: at(now, -4*time.Hour), Kind: KindAck, Session: "b", How: AckNone},
	}
	if r := ResponseRate(events, now); r.Answered != 0 {
		t.Errorf("answered = %d, want 0", r.Answered)
	}
}

func TestRollingWindowExcludesOldNudges(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{TS: at(now, -40*24*time.Hour), Kind: KindNudge, Session: "old"},
		{TS: at(now, -1*time.Hour), Kind: KindNudge, Session: "new"},
	}
	if r := ResponseRate(events, now); r.Nudges != 1 {
		t.Errorf("nudges = %d, want 1 (30-day window)", r.Nudges)
	}
}

func TestOutstanding(t *testing.T) {
	events := []Event{
		{TS: "1", Kind: KindNudge, Session: "a", Fingerprint: "f1"},
	}
	if _, open := Outstanding(events, "a"); !open {
		t.Error("nudge with no ack should be outstanding")
	}
	events = append(events, Event{TS: "2", Kind: KindAck, Session: "a"})
	if _, open := Outstanding(events, "a"); open {
		t.Error("acked nudge should not be outstanding")
	}
	if _, open := Outstanding(events, "other"); open {
		t.Error("another session's nudge should not be outstanding here")
	}
}

// Two Stop hooks finishing together in one worktree both read an empty cooldown
// and both nudged for the same file set: two prompts for one change, and an
// inflated denominator under the rate that gates M3 (prd §8.1). The check and the
// write have to be one locked operation.
func TestTryNudgeIsAtomicUnderConcurrency(t *testing.T) {
	root := t.TempDir()
	const n = 16

	var wg sync.WaitGroup
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := TryNudge(root, "wt", Event{
				Kind: KindNudge, Session: "s", Fingerprint: "same-set", Files: 2,
			}, 15*time.Minute)
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)

	nudged := 0
	for ok := range results {
		if ok {
			nudged++
		}
	}
	if nudged != 1 {
		t.Errorf("%d of %d callers nudged; exactly one should have", nudged, n)
	}
	events, err := Load(root, "wt")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Errorf("ledger holds %d nudges for one file set, want 1", len(events))
	}
}

func TestTryNudgeAllowsADifferentFileSet(t *testing.T) {
	root := t.TempDir()
	if ok, _ := TryNudge(root, "wt", Event{Kind: KindNudge, Session: "s", Fingerprint: "a"}, time.Hour); !ok {
		t.Fatal("first nudge should fire")
	}
	if ok, _ := TryNudge(root, "wt", Event{Kind: KindNudge, Session: "s", Fingerprint: "a"}, time.Hour); ok {
		t.Error("same fingerprint inside the cooldown should be suppressed")
	}
	// A different changed-file set is a different question, so it is not suppressed.
	if ok, _ := TryNudge(root, "wt", Event{Kind: KindNudge, Session: "s", Fingerprint: "b"}, time.Hour); !ok {
		t.Error("a new fingerprint should fire even inside the cooldown")
	}
}

// Fingerprints must not depend on the order git happened to report files.
func TestFingerprintIsOrderIndependent(t *testing.T) {
	a := Fingerprint([]string{"b.go", "a.go"})
	b := Fingerprint([]string{"a.go", "b.go"})
	if a != b {
		t.Errorf("%s != %s; cooldown would not suppress a repeat nudge", a, b)
	}
	if a == Fingerprint([]string{"a.go"}) {
		t.Error("different file sets must fingerprint differently")
	}
}

// Two worktrees must not share nudge state: their changed-file sets differ, so a
// suppression in one would silence a genuine nudge in the other (prd §5.2).
func TestLedgerIsKeyedPerWorktree(t *testing.T) {
	root := t.TempDir()
	if err := Append(root, "wt-a", Event{Kind: KindNudge, Session: "s", Fingerprint: "f"}); err != nil {
		t.Fatal(err)
	}
	b, err := Load(root, "wt-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Errorf("worktree b sees %d of worktree a's events", len(b))
	}
	all, err := LoadAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("LoadAll = %d, want 1", len(all))
	}
}
