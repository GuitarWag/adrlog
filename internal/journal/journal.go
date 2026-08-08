// Package journal records one line per agent turn (prd §6.2).
//
// This is the differentiator: a subagent finishes, returns a summary, and its
// context is discarded — whatever it considered and rejected is gone unless
// something wrote it down first (prd §2, §3).
package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Dir is where journals live, relative to the shared root.
const Dir = ".dlog/journal"

// SummaryLimit caps the stored summary. Entries stay small because the full
// reasoning is reachable through the transcript path (prd §6.2).
const SummaryLimit = 1200

type Entry struct {
	Seq          int      `json:"seq"`
	TS           string   `json:"ts"`
	Event        string   `json:"event"`
	Session      string   `json:"session"`
	AgentType    string   `json:"agent_type,omitempty"`
	AgentID      string   `json:"agent_id,omitempty"`
	PromptID     string   `json:"prompt_id,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	Worktree     string   `json:"worktree,omitempty"`
	Head         string   `json:"head,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	Transcript   string   `json:"transcript,omitempty"`
}

// Ref is the session#seq pointer an ADR's journal_refs holds.
func (e Entry) Ref() string { return e.Session + "#" + strconv.Itoa(e.Seq) }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// FilePath is the journal for one session, under the shared root so five
// worktrees write to one place (prd §5.2).
func FilePath(root, session string) string {
	if session == "" {
		session = "unknown"
	}
	return filepath.Join(root, Dir, unsafeName.ReplaceAllString(session, "_")+".jsonl")
}

// Append writes one entry, assigning the next seq for the session.
//
// One file per session is not one writer per file: three parallel subagents in a
// single session all land here at once, and seq has to stay monotonic and unique
// because journal_refs points at it. So the read-count-then-write is done under
// an exclusive flock; O_APPEND alone would order the writes but hand every
// concurrent caller the same seq.
func Append(root string, e Entry) (Entry, error) {
	path := FilePath(root, e.Session)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return e, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return e, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return e, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	last, err := highWaterSeq(f)
	if err != nil {
		return e, err
	}
	e.Seq = last + 1
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339)
	}
	e.Summary = Truncate(e.Summary, SummaryLimit)

	line, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	// No fsync. The write is one small append under a lock, so another process
	// sees it immediately; only a machine crash could lose it, and the hook budget
	// is 50ms at p99 (prd G7) which an fsync per turn eats into for a journal that
	// is advisory by design.
	_, err = f.Write(append(line, '\n'))
	return e, err
}

// tailWindow is how much of the end of the journal the seq scan reads. Large
// enough to hold many entries even at the 1200-byte summary cap, so the walk back
// to the last readable line effectively always succeeds.
const tailWindow = 256 * 1024

// highWaterSeq returns the largest seq already in the file.
//
// Only the tail is read. Scanning the whole file made every append O(entries),
// which at ten thousand lines put the Stop hook on the 50ms budget line (prd G7)
// — and it did it while holding the exclusive lock, so it slowed every other
// subagent appending at the same time.
//
// A torn final line — a machine that died mid-append — is counted rather than
// skipped. Skipping it would hand its seq to the next entry, and journal_refs
// points at seq to name one specific turn (prd §6.2).
func highWaterSeq(f *os.File) (int, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	start := int64(0)
	if size > tailWindow {
		start = size - tailWindow
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return 0, err
	}

	lines := strings.Split(string(buf), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the window almost certainly began mid-line
	}
	unreadable := 0
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var prev Entry
		if err := json.Unmarshal([]byte(line), &prev); err != nil {
			unreadable++
			continue
		}
		return prev.Seq + unreadable, nil
	}
	// Nothing readable in the window: fall back to counting every line, which is
	// correct if slow, and only happens on a file that is entirely corrupt.
	if start > 0 {
		return countLines(f)
	}
	return unreadable, nil
}

func countLines(f *os.File) (int, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n, sc.Err()
}

// Truncate cuts to a byte budget without splitting a rune.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

// Broken is a journal line that would not parse. Reported rather than skipped,
// for the same reason an unreadable ADR is (prd §6.1).
type Broken struct {
	Path string
	Line int
	Err  error
}

// LoadSession reads one session's entries in file order.
func LoadSession(root, session string) ([]Entry, []Broken, error) {
	return readFile(FilePath(root, session))
}

// LoadAll reads every session's journal, sorted by timestamp then seq.
func LoadAll(root string) ([]Entry, []Broken, error) {
	paths, err := filepath.Glob(filepath.Join(root, Dir, "*.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(paths)
	var all []Entry
	var broken []Broken
	for _, p := range paths {
		entries, b, err := readFile(p)
		if err != nil {
			return nil, nil, err
		}
		all = append(all, entries...)
		broken = append(broken, b...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].TS != all[j].TS {
			return all[i].TS < all[j].TS
		}
		return all[i].Seq < all[j].Seq
	})
	return all, broken, nil
}

func readFile(path string) ([]Entry, []Broken, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	var entries []Entry
	var broken []Broken
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			broken = append(broken, Broken{path, n, err})
			continue
		}
		entries = append(entries, e)
	}
	return entries, broken, sc.Err()
}

// Query filters entries. Zero values mean "no filter".
type Query struct {
	Session string
	Agent   string
	Grep    string
	Since   time.Time
}

func (q Query) Match(e Entry) bool {
	if q.Session != "" && e.Session != q.Session {
		return false
	}
	if q.Agent != "" && e.AgentType != q.Agent {
		return false
	}
	if q.Grep != "" && !strings.Contains(strings.ToLower(e.Summary), strings.ToLower(q.Grep)) {
		return false
	}
	if !q.Since.IsZero() {
		ts, err := time.Parse(time.RFC3339, e.TS)
		if err != nil || ts.Before(q.Since) {
			return false
		}
	}
	return true
}

// Filter applies a query.
func Filter(entries []Entry, q Query) []Entry {
	var out []Entry
	for _, e := range entries {
		if q.Match(e) {
			out = append(out, e)
		}
	}
	return out
}

// Export renders one session as a readable markdown trace, so a session can be
// pasted into a PR comment without committing every journal (prd §6.2).
func Export(session string, entries []Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Session %s\n\n", session)
	if len(entries) == 0 {
		b.WriteString("No entries.\n")
		return b.String()
	}
	for _, e := range entries {
		who := e.AgentType
		if who == "" {
			who = "session"
		}
		fmt.Fprintf(&b, "## %d · %s · %s\n\n", e.Seq, e.Event, who)
		fmt.Fprintf(&b, "- when: %s\n", e.TS)
		if e.Branch != "" || e.Head != "" {
			fmt.Fprintf(&b, "- where: %s @ %s\n", e.Branch, e.Head)
		}
		if len(e.ChangedFiles) > 0 {
			fmt.Fprintf(&b, "- changed: %s\n", strings.Join(e.ChangedFiles, ", "))
		}
		if e.Transcript != "" {
			fmt.Fprintf(&b, "- transcript: %s\n", e.Transcript)
		}
		if e.Summary != "" {
			fmt.Fprintf(&b, "\n%s\n", e.Summary)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RefResolver reports whether a session#seq pointer resolves to a real entry.
// Lint asks only this, never whether the pointer is apt (prd §6.3).
func RefResolver(entries []Entry) func(string) bool {
	known := map[string]bool{}
	for _, e := range entries {
		known[e.Ref()] = true
	}
	return func(ref string) bool { return known[ref] }
}
