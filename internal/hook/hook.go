// Package hook implements the Claude Code lifecycle events (prd §8).
//
// One binary, five subcommands. Two rules run through all of it: a hook says
// nothing when there is nothing to say (prd G7), and a hook never fails the turn
// it is observing — tracking is best-effort, the user's work is not.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GuitarWag/adrlog/internal/adr"
	"github.com/GuitarWag/adrlog/internal/config"
	"github.com/GuitarWag/adrlog/internal/gitx"
	"github.com/GuitarWag/adrlog/internal/globs"
	"github.com/GuitarWag/adrlog/internal/journal"
	"github.com/GuitarWag/adrlog/internal/state"
)

// Events, as passed to `adrlog hook <event>`.
const (
	SessionStart = "session-start"
	Stop         = "stop"
	SubagentStop = "subagent-stop"
	PreCompact   = "pre-compact"
	PostToolUse  = "post-tool-use"
)

// changedFileCap bounds what one entry records. Five sessions of real work a day
// is a lot of lines, and the transcript path is there for the full picture
// (prd §14).
const changedFileCap = 50

// Payload is the hook input on stdin.
//
// Every field is optional on purpose. PreCompact's shape differs from the Stop
// family and may carry no closing assistant message (prd §8, §16.1), so a missing
// field has to degrade to an absent value — never to a fabricated one that reads
// like real data.
type Payload struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	CWD                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	AgentType            string          `json:"agent_type"`
	AgentID              string          `json:"agent_id"`
	PromptID             string          `json:"prompt_id"`
	Trigger              string          `json:"trigger"`
	StopHookActive       bool            `json:"stop_hook_active"`
	ToolInput            json.RawMessage `json:"tool_input"`
}

// OptedIn reports whether a repository has asked to be tracked, which it does by
// having a .adrlog/ directory at the shared root.
//
// The marker is a directory rather than a config file because config is optional
// (prd §6.4) — every field has a default — so requiring one to switch tracking on
// would mean inventing a file with nothing in it.
func OptedIn(root string) bool {
	info, err := os.Stat(filepath.Join(root, ".adrlog"))
	return err == nil && info.IsDir()
}

// Session prefers the payload, then the environment (prd §6.3).
func (p Payload) Session() string {
	if p.SessionID != "" {
		return p.SessionID
	}
	return os.Getenv("CLAUDE_CODE_SESSION_ID")
}

// eventName maps the subcommand to the name recorded in the journal.
var eventName = map[string]string{
	SessionStart: "SessionStart",
	Stop:         "Stop",
	SubagentStop: "SubagentStop",
	PreCompact:   "PreCompact",
	PostToolUse:  "PostToolUse",
}

// Run executes one hook event and returns the process exit code.
//
// It swallows its own errors to stderr rather than returning them: a failure to
// journal must not fail the turn. The one thing it must never do is fail
// silently — invisible tracking loss is the worst shape this can take (prd §12).
func Run(event string, stdin io.Reader, stdout, stderr io.Writer) int {
	if _, ok := eventName[event]; !ok {
		fmt.Fprintf(stderr, "adrlog hook: unknown event %q\n", event)
		return 1
	}

	var p Payload
	if data, err := io.ReadAll(stdin); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &p); err != nil {
			fmt.Fprintf(stderr, "adrlog hook %s: unreadable payload: %v\n", event, err)
			return 0
		}
	}

	// Installed globally, these hooks fire in every session, including ones with
	// no repository at all. Not a git repo is not a problem to report, it is just
	// nothing to do.
	repo, err := gitx.Open(p.CWD)
	if err != nil {
		return 0
	}
	// A repository opts in by having .adrlog/. Without this, a global install would
	// apply the default watch list everywhere and nudge in repos nobody asked to
	// track — and §4 is clear that spurious nudges are how the log becomes
	// wallpaper and the response rate stops meaning anything.
	//
	// Silence here is a deliberate exception to "never fail silently" (prd §12),
	// because an un-opted repo is not tracking-lost, it is tracking-never-asked-for.
	// `adrlog new` creates .adrlog/, so writing one record switches a repo on.
	if !OptedIn(repo.Root()) {
		return 0
	}
	cfg, err := config.Load(repo.Root())
	if err != nil {
		fmt.Fprintf(stderr, "adrlog hook %s: %v\n", event, err)
	}

	switch event {
	case SessionStart:
		return sessionStart(repo, cfg, p, stdout, stderr)
	case PostToolUse:
		return postToolUse(repo, p, stdout, stderr)
	case Stop:
		return stop(repo, cfg, p, stdout, stderr)
	default: // SubagentStop, PreCompact: journal only, never a nudge.
		record(repo, cfg, p, eventName[event], stderr)
		return 0
	}
}

// record appends the turn to the journal. Best effort, loudly on stderr if it
// fails, because a journal that quietly stops is the failure nobody notices.
func record(repo *gitx.Repo, cfg config.Config, p Payload, event string, stderr io.Writer) (journal.Entry, bool) {
	changed, err := repo.ChangedFiles()
	if err != nil {
		fmt.Fprintf(stderr, "adrlog hook: reading changed files: %v\n", err)
	}
	changed = notIgnored(cfg, changed)
	if len(changed) > changedFileCap {
		changed = changed[:changedFileCap]
	}

	e := journal.Entry{
		Event:        event,
		Session:      p.Session(),
		AgentType:    p.AgentType,
		AgentID:      p.AgentID,
		PromptID:     p.PromptID,
		Branch:       repo.Branch(),
		Worktree:     repo.WorktreeName(),
		Head:         repo.Head(),
		ChangedFiles: changed,
		Summary:      p.LastAssistantMessage,
		Transcript:   p.TranscriptPath,
	}
	written, err := journal.Append(repo.Root(), e)
	if err != nil {
		fmt.Fprintf(stderr, "adrlog hook: journal append failed: %v\n", err)
		return e, false
	}
	return written, true
}

func sessionStart(repo *gitx.Repo, cfg config.Config, p Payload, stdout, stderr io.Writer) int {
	var lines []string

	recs, broken, err := adr.LoadAll(repo.Root())
	if err != nil {
		fmt.Fprintf(stderr, "adrlog hook session-start: %v\n", err)
	}
	var proposed []string
	for _, r := range recs {
		if r.Status == "proposed" {
			proposed = append(proposed, fmt.Sprintf("  %s — %s", r.ID, r.Title))
		}
	}
	if len(proposed) > 0 {
		sort.Strings(proposed)
		lines = append(lines, fmt.Sprintf("Open proposals (%d):", len(proposed)))
		lines = append(lines, proposed...)
	}
	if len(broken) > 0 {
		// Not a silent skip, here least of all: a record nothing can read is
		// invisible to every query until someone is told (prd §6.1).
		lines = append(lines, fmt.Sprintf("%d decision record(s) cannot be parsed; run `adrlog lint`.", len(broken)))
	}

	if events, err := state.LoadAll(repo.Root()); err == nil {
		if rate := state.ResponseRate(events, time.Now().UTC()); rate.Finding != "" {
			lines = append(lines, fmt.Sprintf("Nudge response rate %.2f over %s (%d/%d). %s",
				rate.Rate, rate.Window, rate.Answered, rate.Nudges, rate.Finding))
		}
	}

	// Silence is the default. The branch is context for the rest, not a reason to
	// speak on its own (prd G7).
	if len(lines) == 0 {
		return 0
	}
	if b := repo.Branch(); b != "" {
		lines = append(lines, "Branch: "+b)
	}
	writeContext(stdout, "SessionStart", strings.Join(lines, "\n"))
	return 0
}

func stop(repo *gitx.Repo, cfg config.Config, p Payload, stdout, stderr io.Writer) int {
	record(repo, cfg, p, eventName[Stop], stderr)

	session := p.Session()
	worktree := repo.WorktreeName()
	root := repo.Root()

	events, err := state.Load(root, worktree)
	if err != nil {
		fmt.Fprintf(stderr, "adrlog hook stop: reading nudge ledger: %v\n", err)
	}

	// Close out a nudge this session answered by writing a record. `adrlog new`
	// already acks when it knows the session, so this is the fallback for a record
	// written by hand. The other answer, an explicit decline, arrives through
	// `adrlog ack --none`.
	if nudge, open := state.Outstanding(events, session); open && session != "" {
		if since, err := time.Parse(time.RFC3339, nudge.TS); err == nil && recordedBy(root, session, since) {
			if err := state.Append(root, worktree, state.Event{Kind: state.KindAck, Session: session, How: state.AckADR}); err == nil {
				events = append(events, state.Event{TS: time.Now().UTC().Format(time.RFC3339), Kind: state.KindAck, Session: session, How: state.AckADR})
			}
		}
	}

	changed, err := repo.ChangedFiles()
	if err != nil {
		fmt.Fprintf(stderr, "adrlog hook stop: %v\n", err)
		return 0
	}
	watched := watchedFiles(cfg, changed)

	// Suppression, so the nudge stays rare enough to remain worth reading (prd §8).
	if len(watched) < cfg.MinFiles {
		return 0
	}
	for _, f := range changed {
		// Already writing a record; asking for one would be noise.
		if globs.Match(adr.Dir+"/**", f) {
			return 0
		}
	}
	// The cooldown check and the record of the nudge are one locked operation, so
	// two Stop hooks finishing together cannot both decide the coast is clear.
	nudged, err := state.TryNudge(root, worktree, state.Event{
		Kind: state.KindNudge, Session: session, Fingerprint: state.Fingerprint(watched), Files: len(watched),
	}, time.Duration(cfg.CooldownSeconds)*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "adrlog hook stop: recording nudge: %v\n", err)
		return 0
	}
	if !nudged {
		return 0
	}

	msg := nudgeMessage(watched)
	if cfg.Enforce {
		out, _ := json.Marshal(map[string]any{"decision": "block", "reason": msg})
		fmt.Fprintln(stdout, string(out))
		return 0
	}
	writeContext(stdout, "Stop", msg)
	return 0
}

func nudgeMessage(watched []string) string {
	show := watched
	if len(show) > 10 {
		show = show[:10]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d watched files changed this session with no decision record:\n", len(watched))
	for _, f := range show {
		b.WriteString("  " + f + "\n")
	}
	if len(watched) > len(show) {
		fmt.Fprintf(&b, "  … and %d more\n", len(watched)-len(show))
	}
	// Both branches are offered plainly. A record written to clear a prompt is
	// worse than no record (prd §4), so declining has to be as easy as complying.
	b.WriteString("\nIf a design decision was made here, write it down: `adrlog new \"<title>\" --affects '<glob>'`.\n")
	b.WriteString("If nothing was decided, say so and run `adrlog ack --none` — that is a complete answer, and it keeps the log honest.")
	return b.String()
}

func postToolUse(repo *gitx.Repo, p Payload, stdout, stderr io.Writer) int {
	// The settings matcher scopes this to docs/adr/**; re-check so a broader
	// matcher cannot turn every edit in the repo into a lint run.
	if path := toolPath(p); path != "" {
		rel, err := filepath.Rel(repo.Toplevel(), path)
		if err != nil || !globs.Match(adr.Dir+"/**", filepath.ToSlash(rel)) {
			return 0
		}
	}

	root := repo.Root()
	recs, broken, err := adr.LoadAll(root)
	if err != nil {
		fmt.Fprintf(stderr, "adrlog hook post-tool-use: %v\n", err)
		return 0
	}

	// Publish the index only when every record was readable. Regenerating first
	// meant an unreadable record was dropped from the human-facing README and
	// *then* reported — the index on disk was already wrong by the time anyone
	// heard about it, which is the silent skip §6.1 rules out wearing a hat.
	if len(broken) == 0 {
		if err := adr.WriteIndex(root, recs); err != nil {
			fmt.Fprintf(stderr, "adrlog hook post-tool-use: index: %v\n", err)
		}
	}

	// No Tracked, so no rot check. This hook reports errors only — the warnings
	// below are dropped on the floor — and scanning every tracked file for every
	// affects glob to produce them was most of this hook's runtime. `adrlog lint`
	// still runs the full check for the human and for CI, where no budget applies.
	findings := adr.Lint(recs, broken, adr.Options{
		RefExists:    refResolver(root, recs),
		KnownSession: func(s string) bool { return journal.SessionExists(root, s) },
	})
	if !adr.HasErrors(findings) {
		return 0
	}
	// Exit 2 hands the defect back to Claude on stderr (prd §8).
	fmt.Fprintln(stderr, "adrlog lint found problems in the record you just wrote:")
	for _, f := range findings {
		if f.Level == adr.Error {
			fmt.Fprintf(stderr, "  %s: %s\n", filepath.Base(f.Path), f.Msg)
		}
	}
	return 2
}

// refResolver reads only the sessions the records actually point at, and nothing
// at all when they point at none. Loading every journal to answer no questions,
// or to answer two, was most of this hook's runtime against a 50ms budget (G7).
func refResolver(root string, recs []*adr.Record) func(string) bool {
	var refs []string
	for _, r := range recs {
		refs = append(refs, r.JournalRefs...)
	}
	return journal.RefResolverFor(root, refs)
}

func toolPath(p Payload) string {
	if len(p.ToolInput) == 0 {
		return ""
	}
	var in struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(p.ToolInput, &in) != nil {
		return ""
	}
	return in.FilePath
}

// recordedBy reports whether this session wrote a record at or after t.
//
// Both halves matter, and an earlier version had neither. Matching on file mtime
// alone meant a `git checkout`, a `git stash pop`, or any unrelated session's
// record answered every open nudge in the repository — with five worktrees
// sharing one docs/adr/, one record a day pinned the response rate near 1.0 and
// hid the exact failure §8.1 exists to expose. The session field is what ties a
// record to the nudge it answers; §8.1 says "within the same session" and means it.
func recordedBy(root, session string, t time.Time) bool {
	recs, _, err := adr.LoadAll(root)
	if err != nil {
		return false
	}
	for _, r := range recs {
		if r.Session != session {
			continue
		}
		// Nudge timestamps are second-precision, as are ids, so a record created
		// within the nudge's own second counts.
		if created, ok := r.Created(); ok && !created.Before(t.Truncate(time.Second)) {
			return true
		}
	}
	return false
}

// watchedFiles keeps paths in the watch list and out of the ignore list.
func watchedFiles(cfg config.Config, files []string) []string {
	var out []string
	for _, f := range files {
		if globs.MatchAny(cfg.Ignore, f) {
			continue
		}
		if globs.MatchAny(cfg.Watch, f) {
			out = append(out, f)
		}
	}
	return out
}

// notIgnored drops only the ignore list. The journal keeps a wider view than the
// nudge does: journal_refs inference matches an ADR's affects globs against these
// paths, and an affects glob is free to point outside the watch list.
func notIgnored(cfg config.Config, files []string) []string {
	var out []string
	for _, f := range files {
		if !globs.MatchAny(cfg.Ignore, f) {
			out = append(out, f)
		}
	}
	return out
}

func writeContext(w io.Writer, event, text string) {
	out, err := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": text,
		},
	})
	if err != nil {
		return
	}
	fmt.Fprintln(w, string(out))
}
