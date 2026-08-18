// Package state keeps the per-worktree nudge ledger (prd §8.1).
//
// The ledger exists so one specific failure is visible: a non-blocking nudge that
// the agent declines every time, forever, leaves every other metric green while
// the tool quietly stops working. This is an instrument, not an enforcement — the
// number's job is to make the failure showable, and it gates M3+ (prd §13).
package state

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Kinds of ledger event.
const (
	KindNudge = "nudge"
	KindAck   = "ack"
)

// How a nudge was answered.
const (
	AckADR  = "adr"  // an ADR appeared in the session
	AckNone = "none" // the agent explicitly declined, via `adrlog ack --none`
)

// ResponseWindow is the rolling window for the response rate (prd §8.1).
const ResponseWindow = 30 * 24 * time.Hour

// ResponseFloor is the rate the response rate has to clear. A guess, and recorded
// as one (prd §16.4) — it lives here rather than in config because changing it
// changes what the M3 gate means.
//
// The gate is "above 0.5" (prd G5, §13) while §8.1 words the finding as "below
// 0.5", which leaves exactly 0.5 failing the gate without reporting anything. The
// finding follows the gate: it fires whenever the rate is not above the floor, so
// the instrument and the thing it gates cannot disagree.
const ResponseFloor = 0.5

type Event struct {
	TS          string `json:"ts"`
	Kind        string `json:"kind"`
	Session     string `json:"session"`
	Fingerprint string `json:"fingerprint,omitempty"`
	How         string `json:"how,omitempty"`
	Files       int    `json:"files,omitempty"`
}

// Dir is the per-worktree state directory (prd §5.2). Keyed by worktree because
// a nudge fingerprint from worktree A must not suppress a nudge in worktree B:
// their changed-file sets differ, so sharing this would be wrong, not just racy.
func Dir(root, worktree string) string {
	return filepath.Join(root, ".adrlog", "state", worktree)
}

func ledgerPath(root, worktree string) string {
	return filepath.Join(Dir(root, worktree), "nudges.jsonl")
}

// Fingerprint identifies a changed-file set, so the same set does not nudge twice
// inside the cooldown.
func Fingerprint(files []string) string {
	s := append([]string(nil), files...)
	sort.Strings(s)
	sum := sha256.Sum256([]byte(strings.Join(s, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// Append records a ledger event.
func Append(root, worktree string, e Event) error {
	f, err := openLedger(root, worktree)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return write(f, e)
}

// TryNudge records a nudge unless one for the same fingerprint is still inside
// the cooldown, reporting whether it wrote.
//
// The check and the write are one locked operation. Split across a read and a
// later Append, two Stop hooks finishing together in one worktree both saw an
// empty cooldown and both nudged for the same file set — two prompts for one
// change, and an inflated denominator under the rate that gates M3 (prd §8.1).
func TryNudge(root, worktree string, e Event, cooldown time.Duration) (bool, error) {
	f, err := openLedger(root, worktree)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	events, err := readEvents(f)
	if err != nil {
		return false, err
	}
	if last, ok := LastNudge(events, e.Fingerprint); ok {
		if ts, err := time.Parse(time.RFC3339, last.TS); err == nil && time.Since(ts) < cooldown {
			return false, nil
		}
	}
	if err := write(f, e); err != nil {
		return false, err
	}
	return true, nil
}

func openLedger(root, worktree string) (*os.File, error) {
	path := ledgerPath(root, worktree)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
}

func write(f *os.File, e Event) error {
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// Load reads one worktree's ledger.
func Load(root, worktree string) ([]Event, error) {
	return readLedger(ledgerPath(root, worktree))
}

// LoadAll reads every worktree's ledger, for the repo-wide response rate.
func LoadAll(root string) ([]Event, error) {
	dirs, err := filepath.Glob(filepath.Join(root, ".adrlog", "state", "*", "nudges.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	var all []Event
	for _, d := range dirs {
		evs, err := readLedger(d)
		if err != nil {
			return nil, err
		}
		all = append(all, evs...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	return all, nil
}

func readLedger(path string) ([]Event, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readEvents(f)
}

func readEvents(f *os.File) ([]Event, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var out []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		// A corrupt ledger line is dropped rather than failing the hook: this is an
		// instrument, and a broken instrument must not break the turn it measures.
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// LastNudge returns the most recent nudge for a fingerprint, if any.
func LastNudge(events []Event, fingerprint string) (Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == KindNudge && events[i].Fingerprint == fingerprint {
			return events[i], true
		}
	}
	return Event{}, false
}

// Outstanding returns the most recent nudge in a session that no later ack in
// that same session has answered.
func Outstanding(events []Event, session string) (Event, bool) {
	var last Event
	var have bool
	for _, e := range events {
		if e.Session != session {
			continue
		}
		switch e.Kind {
		case KindNudge:
			last, have = e, true
		case KindAck:
			have = false
		}
	}
	return last, have
}

// Rate is the rolling response rate (prd §8.1). A nudge counts as answered when
// an ack lands in the same session at or after it.
type Rate struct {
	Nudges   int     `json:"nudges"`
	Answered int     `json:"answered"`
	Rate     float64 `json:"rate"`
	Window   string  `json:"window"`
	Finding  string  `json:"finding,omitempty"`
}

// ResponseRate computes the rate over the window ending at now.
func ResponseRate(events []Event, now time.Time) Rate {
	cutoff := now.Add(-ResponseWindow)
	r := Rate{Window: "30d"}

	// Acks, per session, ordered. A nudge is answered by the first ack in its
	// session at or after it — one ack does not clear a backlog of nudges.
	acks := map[string][]time.Time{}
	for _, e := range events {
		if e.Kind != KindAck {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, e.TS); err == nil {
			acks[e.Session] = append(acks[e.Session], ts)
		}
	}
	used := map[string]int{}

	for _, e := range events {
		if e.Kind != KindNudge {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.TS)
		if err != nil || ts.Before(cutoff) {
			continue
		}
		r.Nudges++
		for i := used[e.Session]; i < len(acks[e.Session]); i++ {
			if !acks[e.Session][i].Before(ts) {
				r.Answered++
				used[e.Session] = i + 1
				break
			}
		}
	}
	if r.Nudges > 0 {
		r.Rate = float64(r.Answered) / float64(r.Nudges)
		if r.Rate <= ResponseFloor {
			// Both readings demand a human looking at the config, not more automation.
			r.Finding = "nudge response rate below 0.5: either the watch list is too broad and the nudges are spurious, or the log has become wallpaper"
		}
	}
	return r
}
