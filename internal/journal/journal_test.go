package journal

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Three parallel subagents in one session all append to one file, so "one writer
// per file" does not hold within a session. O_APPEND alone would order
// the writes but hand every concurrent caller the same seq, and journal_refs
// points at seq — duplicates would make those pointers ambiguous. This pins the
// lock, and it is the M2 done-condition in miniature (docs/future-work.md).
func TestConcurrentAppendGivesUniqueSeq(t *testing.T) {
	root := t.TempDir()
	const n = 24

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := Append(root, Entry{
				Event:     "SubagentStop",
				Session:   "sess1",
				AgentType: fmt.Sprintf("agent-%d", i),
				Summary:   fmt.Sprintf("summary %d", i),
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	entries, broken, err := LoadSession(root, "sess1")
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) > 0 {
		t.Fatalf("%d unreadable lines — interleaved writes corrupted the file", len(broken))
	}
	if len(entries) != n {
		t.Fatalf("got %d entries, want %d — writes were lost", len(entries), n)
	}
	seen := map[int]bool{}
	for _, e := range entries {
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d; journal_refs would be ambiguous", e.Seq)
		}
		seen[e.Seq] = true
	}
	for i := 1; i <= n; i++ {
		if !seen[i] {
			t.Errorf("seq %d missing; sequence is not monotonic", i)
		}
	}
}

func TestSeparateSessionsSeparateFiles(t *testing.T) {
	root := t.TempDir()
	Append(root, Entry{Session: "a", Event: "Stop", Summary: "one"})
	Append(root, Entry{Session: "b", Event: "Stop", Summary: "two"})

	a, _, _ := LoadSession(root, "a")
	if len(a) != 1 || a[0].Seq != 1 {
		t.Errorf("session a = %+v; seq must restart per session", a)
	}
	all, _, _ := LoadAll(root)
	if len(all) != 2 {
		t.Errorf("LoadAll = %d entries, want 2", len(all))
	}
}

// A session id arriving from a hook payload is untrusted input; it must not be
// able to steer the write out of the journal directory.
func TestFilePathSanitisesSession(t *testing.T) {
	got := FilePath("/root", "../../etc/passwd")
	// The separators are what matter: the id may keep dots in its flattened name
	// as long as it cannot climb out of the directory.
	if dir := filepath.Dir(got); dir != filepath.Join("/root", Dir) {
		t.Errorf("FilePath escaped the journal directory: %s", got)
	}
	if got != filepath.Clean(got) {
		t.Errorf("FilePath is not a clean path: %s", got)
	}
}

func TestTruncateKeepsRunesIntact(t *testing.T) {
	s := strings.Repeat("é", 20) // two bytes per rune
	got := Truncate(s, 15)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation marker, got %q", got)
	}
	for _, r := range got {
		if r == '�' {
			t.Fatalf("truncation split a rune: %q", got)
		}
	}
	if short := Truncate("abc", 10); short != "abc" {
		t.Errorf("Truncate shortened a fitting string: %q", short)
	}
}

func TestAppendTruncatesSummary(t *testing.T) {
	root := t.TempDir()
	e, err := Append(root, Entry{Session: "s", Event: "Stop", Summary: strings.Repeat("x", SummaryLimit+500)})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Summary) > SummaryLimit+4 {
		t.Errorf("summary is %d bytes, cap is %d", len(e.Summary), SummaryLimit)
	}
}

// A corrupt line must be reported, not skipped into invisibility. Unreadable is
// a defect, never an absence.
func TestBrokenLineIsReported(t *testing.T) {
	root := t.TempDir()
	Append(root, Entry{Session: "s", Event: "Stop", Summary: "fine"})
	path := FilePath(root, "s")
	appendRaw(t, path, "{not json\n")

	entries, broken, err := LoadSession(root, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d good entries, want 1", len(entries))
	}
	if len(broken) != 1 || broken[0].Line != 2 {
		t.Errorf("broken = %+v, want one finding on line 2", broken)
	}
}

// A machine that dies mid-append leaves a torn line. Skipping it when computing
// the next seq handed its number to the following entry, and journal_refs points
// at seq to name one specific turn.
func TestTornLineDoesNotReuseSeq(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		Append(root, Entry{Session: "s", Event: "Stop", Summary: "entry"})
	}
	path := FilePath(root, "s")
	appendRaw(t, path, `{"seq":4,"ts":"2026-08-06T10:3`) // torn, no newline

	e, err := Append(root, Entry{Session: "s", Event: "Stop", Summary: "after the tear"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq <= 4 {
		t.Errorf("seq %d reuses or precedes the torn entry's number", e.Seq)
	}
	entries, broken, _ := LoadSession(root, "s")
	if len(broken) != 1 {
		t.Errorf("expected the torn line reported, got %d", len(broken))
	}
	seen := map[int]bool{}
	for _, x := range entries {
		if seen[x.Seq] {
			t.Errorf("duplicate seq %d", x.Seq)
		}
		seen[x.Seq] = true
	}
}

// Appends must not get slower as the journal grows: the scan ran under the
// exclusive lock, so it slowed every other subagent appending at the same time,
// and it put the Stop hook on the 50ms budget at ten thousand lines.
func TestAppendReadsOnlyTheTail(t *testing.T) {
	root := t.TempDir()
	if _, err := Append(root, Entry{Session: "big", Event: "Stop", Summary: "first"}); err != nil {
		t.Fatal(err)
	}
	path := FilePath(root, "big")
	// Enough lines to push the early entries well outside the tail window.
	for i := 2; i <= 4000; i++ {
		appendRaw(t, path, fmt.Sprintf(`{"seq":%d,"ts":"2026-08-06T10:00:00Z","event":"Stop","session":"big","summary":"%s"}`+"\n",
			i, strings.Repeat("x", 200)))
	}
	e, err := Append(root, Entry{Session: "big", Event: "Stop", Summary: "last"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 4001 {
		t.Errorf("seq = %d, want 4001; the tail scan lost the high-water mark", e.Seq)
	}
}

func TestRefResolver(t *testing.T) {
	resolve := RefResolver([]Entry{{Session: "s", Seq: 3}})
	if !resolve("s#3") {
		t.Error("s#3 should resolve")
	}
	if resolve("s#4") {
		t.Error("s#4 should not resolve")
	}
}

func TestFilter(t *testing.T) {
	entries := []Entry{
		{Session: "s", Seq: 1, AgentType: "qa", Summary: "Chose a tombstone column", TS: "2026-08-01T00:00:00Z"},
		{Session: "s", Seq: 2, AgentType: "dev", Summary: "Nothing much", TS: "2026-08-05T00:00:00Z"},
	}
	if got := Filter(entries, Query{Agent: "qa"}); len(got) != 1 || got[0].Seq != 1 {
		t.Errorf("agent filter = %+v", got)
	}
	if got := Filter(entries, Query{Grep: "TOMBSTONE"}); len(got) != 1 {
		t.Errorf("grep should be case-insensitive, got %+v", got)
	}
	since, _ := time.Parse(time.RFC3339, "2026-08-03T00:00:00Z")
	if got := Filter(entries, Query{Since: since}); len(got) != 1 || got[0].Seq != 2 {
		t.Errorf("since filter = %+v", got)
	}
}

func TestExportRendersSummaries(t *testing.T) {
	out := Export("sess", []Entry{{Seq: 1, Event: "SubagentStop", AgentType: "qa", TS: "t", Summary: "rejected hard delete"}})
	if !strings.Contains(out, "rejected hard delete") || !strings.Contains(out, "qa") {
		t.Errorf("export lost content:\n%s", out)
	}
}
