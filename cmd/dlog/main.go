// Command dlog records architecture decisions and agent reasoning across
// parallel Claude Code sessions. See prd.md.
//
// Scope: this binary implements M1 and M2 (prd §0, §13). Retrieval, audit and
// drift analysis are gated on evidence from using v0.1, and the commands that
// would front them say so rather than shipping a stub that looks like an answer.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"dlog/internal/adr"
	"dlog/internal/config"
	"dlog/internal/gitx"
	"dlog/internal/globs"
	"dlog/internal/hook"
	"dlog/internal/journal"
	"dlog/internal/state"
)

const usage = `dlog — decision tracking for parallel agent sessions

  dlog new <title>   [--status --supersedes --depends-on --affects --agent --tags --session --no-refs]
  dlog list          [--status --tag --affects]
  dlog show <id>
  dlog lint
  dlog index
  dlog journal       [--session --since --agent --grep --export <session>]
  dlog ack           --none
  dlog drift
  dlog hook <event>  session-start | stop | subagent-stop | pre-compact | post-tool-use

Every command takes --json.
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "new":
		return cmdNew(rest)
	case "list":
		return cmdList(rest)
	case "show":
		return cmdShow(rest)
	case "lint":
		return cmdLint(rest)
	case "index":
		return cmdIndex(rest)
	case "journal":
		return cmdJournal(rest)
	case "ack":
		return cmdAck(rest)
	case "drift":
		return cmdDrift(rest)
	case "hook":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "dlog hook: need an event")
			return 2
		}
		return hook.Run(rest[0], os.Stdin, os.Stdout, os.Stderr)
	case "init", "ctx", "audit", "prune":
		// Gated behind the M3+ evidence gate (prd §0, §13). Saying so beats a stub
		// that returns an empty result indistinguishable from a real one.
		fmt.Fprintf(os.Stderr, "dlog %s: not built yet — gated behind the v0.1 evidence gate (prd §13).\n", cmd)
		return 2
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "dlog: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// open resolves the repo and config together, since every command needs both.
func open() (*gitx.Repo, config.Config, error) {
	repo, err := gitx.Open("")
	if err != nil {
		return nil, config.Config{}, fmt.Errorf("not a git repository: %w", err)
	}
	cfg, err := config.Load(repo.Root())
	return repo, cfg, err
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "dlog:", err)
	return 1
}

func emit(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// parseAfterPositional lets flags follow the positional argument, which is the
// documented shape of `dlog new <title> [flags]` (prd §7). Go's flag package
// stops at the first non-flag token, so parsing the raw args would quietly fold
// every flag into the title — a wrong id and a dropped --supersedes, with no
// error anywhere.
func parseAfterPositional(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	i := 0
	for ; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			break
		}
		positional = append(positional, args[i])
	}
	if err := fs.Parse(args[i:]); err != nil {
		return nil, err
	}
	return append(positional, fs.Args()...), nil
}

// listFlag collects a repeatable flag that also accepts comma-separated values.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func cmdNew(args []string) int {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	var affects, supersedes, dependsOn, tags listFlag
	fs.Var(&affects, "affects", "glob this decision governs (repeatable)")
	fs.Var(&supersedes, "supersedes", "id this decision replaces (repeatable)")
	fs.Var(&dependsOn, "depends-on", "id this decision builds on (repeatable)")
	fs.Var(&tags, "tags", "tag (repeatable)")
	status := fs.String("status", "proposed", "proposed | accepted | rejected | superseded | reverted")
	agent := fs.String("agent", "", "authoring agent, recorded as subagent:<name>")
	session := fs.String("session", "", "session id (defaults to $CLAUDE_CODE_SESSION_ID)")
	noRefs := fs.Bool("no-refs", false, "skip journal_refs inference")
	asJSON := fs.Bool("json", false, "machine-readable output")
	words, err := parseAfterPositional(fs, args)
	if err != nil {
		return 2
	}
	title := strings.TrimSpace(strings.Join(words, " "))
	if title == "" {
		fmt.Fprintln(os.Stderr, "dlog new: need a title")
		return 2
	}
	// A title is untrusted input reaching a line-oriented format. A newline in it
	// wrote extra frontmatter fields and dropped the rest of the title, producing
	// a record that lints clean and describes a different decision.
	if err := adr.CheckScalar("title", title); err != nil {
		fmt.Fprintln(os.Stderr, "dlog new:", err)
		return 2
	}
	if !containsStr(adr.Statuses, *status) {
		fmt.Fprintf(os.Stderr, "dlog new: invalid status %q, want one of %s\n", *status, strings.Join(adr.Statuses, ", "))
		return 2
	}

	repo, cfg, openErr := open()
	if openErr != nil {
		return fail(openErr)
	}
	root := repo.Root()

	sess := *session
	if sess == "" {
		sess = os.Getenv("CLAUDE_CODE_SESSION_ID")
	}
	author := *agent
	if author != "" && !strings.Contains(author, ":") {
		author = "subagent:" + author
	}

	now := time.Now()
	id := adr.NewID(now, title)
	// Collision needs the same second and the same slug, so this is rare — but
	// two agents told to record the same decision at once is exactly the case
	// that produces it, and a silent overwrite would lose one of them (prd §5.1).
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(root, adr.Filename(id))); os.IsNotExist(err) {
			break
		}
		id = adr.NewID(now, title) + "-" + strconv.Itoa(n)
	}

	rec := &adr.Record{
		ID: id, Title: title, Status: *status, Date: now.Format(adr.DateFormat),
		Author: author, Branch: repo.Branch(), Worktree: repo.WorktreeName(), Session: sess,
		Affects: affects, Supersedes: supersedes, DependsOn: dependsOn, Tags: tags,
	}

	var warnings []string
	entries, _, _ := journal.LoadSession(root, sess)

	if !*noRefs && sess != "" {
		rec.JournalRefs = inferRefs(root, sess, entries, affects)
	}
	if sess != "" {
		warnings = append(warnings, birthCheck(cfg, affects, entries)...)
	}

	// Check every target before touching any of them. Mutating as we went meant a
	// second, bogus --supersedes aborted the command after the first target had
	// already been flipped to superseded — status_weight 0.0, so dropped from
	// retrieval entirely (prd §9) — pointing at a record that was never created.
	for _, target := range supersedes {
		if err := checkSupersedeTarget(root, target); err != nil {
			return fail(err)
		}
	}
	for _, target := range supersedes {
		w, err := linkSupersede(repo, root, target, id)
		if err != nil {
			return fail(err)
		}
		warnings = append(warnings, w...)
	}

	if err := rec.Write(root); err != nil {
		return fail(err)
	}

	// Answer an outstanding nudge here, where the session is known for certain,
	// rather than leaving the Stop hook to infer it from the filesystem (prd §8.1).
	if sess != "" {
		if events, err := state.Load(root, repo.WorktreeName()); err == nil {
			if _, open := state.Outstanding(events, sess); open {
				state.Append(root, repo.WorktreeName(), state.Event{
					Kind: state.KindAck, Session: sess, How: state.AckADR,
				})
			}
		}
	}

	if recs, broken, err := adr.LoadAll(root); err == nil {
		// An index built from a set with unreadable records in it is missing
		// records, so say so instead of publishing a quietly incomplete document.
		for _, b := range broken {
			warnings = append(warnings, "not in the index, unreadable: "+b.Err.Error())
		}
		if len(broken) == 0 {
			if err := adr.WriteIndex(root, recs); err != nil {
				warnings = append(warnings, "index regeneration failed: "+err.Error())
			}
		}
	}

	rel := adr.Filename(id)
	if *asJSON {
		emit(map[string]any{
			"id": id, "path": rel, "status": rec.Status,
			"journal_refs": rec.JournalRefs, "warnings": warnings,
		})
		return 0
	}
	fmt.Println(rel)
	if len(rec.JournalRefs) > 0 {
		fmt.Printf("journal_refs: %s\n", strings.Join(rec.JournalRefs, ", "))
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	return 0
}

// inferRefs picks the turns that produced this decision (prd §6.3). Advisory: it
// tells a reader where to look, not what is true, so an over-inclusive answer is
// the right failure direction.
func inferRefs(root, session string, entries []journal.Entry, affects []string) []string {
	var candidates []journal.Entry
	if len(affects) > 0 {
		for _, e := range entries {
			if len(globs.Overlap(affects, e.ChangedFiles)) > 0 {
				candidates = append(candidates, e)
			}
		}
	}
	if len(candidates) == 0 {
		// Fall back to everything since the previous record this session wrote. That
		// boundary is recoverable from the refs it already claimed.
		after := previousRefSeq(root, session)
		for _, e := range entries {
			if e.Seq > after {
				candidates = append(candidates, e)
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Seq > candidates[j].Seq })
	if len(candidates) > 10 {
		candidates = candidates[:10]
	}
	refs := make([]string, 0, len(candidates))
	for _, e := range candidates {
		refs = append(refs, e.Ref())
	}
	return refs
}

func previousRefSeq(root, session string) int {
	recs, _, err := adr.LoadAll(root)
	if err != nil {
		return 0
	}
	max := 0
	for _, r := range recs {
		if r.Session != session {
			continue
		}
		for _, ref := range r.JournalRefs {
			parts := strings.SplitN(ref, "#", 2)
			if len(parts) != 2 || parts[0] != session {
				continue
			}
			if n, err := strconv.Atoi(parts[1]); err == nil && n > max {
				max = n
			}
		}
	}
	return max
}

// birthCheck compares the supplied globs against what the session actually
// touched (prd §5.4). The globs are written at the moment of least certainty, and
// a wrong one corrupts journal_refs, retrieval and drift at once — so the check
// runs at the only moment ground truth is available, and warns rather than fails.
func birthCheck(cfg config.Config, affects []string, entries []journal.Entry) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var changed []string
	for _, e := range entries {
		for _, f := range e.ChangedFiles {
			if !seen[f] {
				seen[f] = true
				changed = append(changed, f)
			}
		}
	}
	if len(changed) == 0 {
		return nil
	}

	var warnings []string
	if len(affects) == 0 {
		return []string{fmt.Sprintf("no --affects given, but this session touched %d file(s); retrieval and drift both key off affects", len(changed))}
	}
	for _, g := range affects {
		if len(globs.Overlap([]string{g}, changed)) == 0 {
			warnings = append(warnings, fmt.Sprintf("affects glob %q matches nothing this session changed", g))
		}
	}
	var unmatched []string
	for _, f := range changed {
		if globs.MatchAny(cfg.Ignore, f) || !globs.MatchAny(cfg.Watch, f) {
			continue
		}
		if !globs.MatchAny(affects, f) {
			unmatched = append(unmatched, f)
		}
	}
	if len(unmatched) > 0 {
		show := unmatched
		if len(show) > 5 {
			show = show[:5]
		}
		warnings = append(warnings, fmt.Sprintf("session changed %d watched file(s) no affects glob covers: %s",
			len(unmatched), strings.Join(show, ", ")))
	}
	return warnings
}

// checkSupersedeTarget verifies a target exists and is readable, without writing.
func checkSupersedeTarget(root, target string) error {
	path := filepath.Join(root, adr.Filename(target))
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("--supersedes %s: %w", target, err)
	}
	if _, err := adr.Parse(path, data); err != nil {
		return fmt.Errorf("--supersedes %s: %w", target, err)
	}
	return nil
}

// linkSupersede writes the reciprocal back-link and flips the target's status.
func linkSupersede(repo *gitx.Repo, root, target, newID string) ([]string, error) {
	rel := adr.Filename(target)
	path := filepath.Join(root, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--supersedes %s: %w", target, err)
	}
	rec, err := adr.Parse(path, data)
	if err != nil {
		return nil, fmt.Errorf("--supersedes %s: %w", target, err)
	}

	var warnings []string
	// The target may be open and dirty in another of five parallel sessions. We
	// still write — lint catches broken reciprocity everywhere afterward, so a lost
	// write is detected rather than silent — but the warning is the cheap half of
	// that guarantee (prd §5.2).
	if repo.IsDirty(rel) {
		warnings = append(warnings, fmt.Sprintf("%s has uncommitted changes; another session may be editing it, verify the back-link survived", rel))
	}

	if !containsStr(rec.SupersededBy, newID) {
		if err := adr.SetList(path, "superseded_by", append(rec.SupersededBy, newID)); err != nil {
			return warnings, err
		}
	}
	if rec.Status != "superseded" {
		if err := adr.SetScalar(path, "status", "superseded"); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	status := fs.String("status", "", "filter by status")
	tag := fs.String("tag", "", "filter by tag")
	affects := fs.String("affects", "", "only records whose affects globs cover this path")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	repo, _, err := open()
	if err != nil {
		return fail(err)
	}
	recs, broken, err := adr.LoadAll(repo.Root())
	if err != nil {
		return fail(err)
	}

	var out []map[string]any
	for _, r := range recs {
		if *status != "" && r.Status != *status {
			continue
		}
		if *tag != "" && !containsStr(r.Tags, *tag) {
			continue
		}
		if *affects != "" && !globs.MatchAny(r.Affects, *affects) {
			continue
		}
		out = append(out, map[string]any{
			"id": r.ID, "title": r.Title, "status": r.Status, "date": r.Date,
			"tags": r.Tags, "affects": r.Affects, "path": adr.Filename(r.ID),
		})
	}

	if *asJSON {
		emit(map[string]any{"records": out, "unreadable": brokenPaths(broken)})
		return 0
	}
	for _, r := range out {
		fmt.Printf("%-10s %s  %s\n", r["status"], r["id"], r["title"])
	}
	// Never let an unreadable record pass as an absent one (prd §6.1).
	for _, b := range broken {
		fmt.Fprintf(os.Stderr, "unreadable: %s\n", b.Err)
	}
	return 0
}

func cmdShow(args []string) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	words, err := parseAfterPositional(fs, args)
	if err != nil {
		return 2
	}
	if len(words) != 1 {
		fmt.Fprintln(os.Stderr, "dlog show: need exactly one id")
		return 2
	}
	repo, _, err := open()
	if err != nil {
		return fail(err)
	}
	path := filepath.Join(repo.Root(), adr.Filename(words[0]))
	data, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	if !*asJSON {
		os.Stdout.Write(data)
		return 0
	}
	rec, err := adr.Parse(path, data)
	if err != nil {
		return fail(err)
	}
	emit(map[string]any{
		"id": rec.ID, "title": rec.Title, "status": rec.Status, "date": rec.Date,
		"author": rec.Author, "branch": rec.Branch, "worktree": rec.Worktree, "session": rec.Session,
		"affects": rec.Affects, "supersedes": rec.Supersedes, "superseded_by": rec.SupersededBy,
		"depends_on": rec.DependsOn, "journal_refs": rec.JournalRefs, "tags": rec.Tags,
		"sections": sectionMap(rec),
	})
	return 0
}

func sectionMap(r *adr.Record) map[string]string {
	m := map[string]string{}
	for _, s := range adr.Sections {
		m[s] = r.Section(s)
	}
	return m
}

func cmdLint(args []string) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	fix := fs.Bool("fix", false, "not implemented (prd M5)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *fix {
		fmt.Fprintln(os.Stderr, "dlog lint --fix: not built yet (prd §13, M5)")
		return 2
	}
	repo, _, err := open()
	if err != nil {
		return fail(err)
	}
	root := repo.Root()
	recs, broken, err := adr.LoadAll(root)
	if err != nil {
		return fail(err)
	}
	tracked, _ := repo.TrackedFiles()
	entries, brokenJournal, _ := journal.LoadAll(root)
	findings := adr.Lint(recs, broken, adr.Options{Tracked: tracked, RefExists: journal.RefResolver(entries)})
	for _, b := range brokenJournal {
		findings = append(findings, adr.Finding{
			Level: adr.Error, Path: b.Path,
			Msg: fmt.Sprintf("unreadable journal line %d: %v", b.Line, b.Err),
		})
	}

	if *asJSON {
		emit(map[string]any{"findings": findings, "records": len(recs), "errors": adr.HasErrors(findings)})
	} else {
		for _, f := range findings {
			fmt.Printf("%-7s %s: %s\n", f.Level, filepath.Base(f.Path), f.Msg)
		}
		if len(findings) == 0 {
			fmt.Printf("%d record(s), no findings\n", len(recs))
		}
	}
	if adr.HasErrors(findings) {
		return 1
	}
	return 0
}

func cmdIndex(args []string) int {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	repo, _, err := open()
	if err != nil {
		return fail(err)
	}
	recs, broken, err := adr.LoadAll(repo.Root())
	if err != nil {
		return fail(err)
	}

	// Refuse rather than publish an index that is missing records nobody was told
	// about (prd §6.1). Writing it anyway was doubly bad: the output is stable
	// across runs, so a CI check for a stale index passes while the index is wrong.
	if len(broken) > 0 {
		if *asJSON {
			emit(map[string]any{"path": adr.IndexPath, "written": false, "unreadable": brokenPaths(broken)})
		} else {
			fmt.Fprintf(os.Stderr, "dlog index: refusing to write, %d record(s) unreadable:\n", len(broken))
			for _, b := range broken {
				fmt.Fprintf(os.Stderr, "  %s\n", b.Err)
			}
		}
		return 1
	}

	if err := adr.WriteIndex(repo.Root(), recs); err != nil {
		return fail(err)
	}
	if *asJSON {
		emit(map[string]any{"path": adr.IndexPath, "written": true, "records": len(recs), "unreadable": []string{}})
	} else {
		fmt.Printf("%s (%d records)\n", adr.IndexPath, len(recs))
	}
	return 0
}

func cmdJournal(args []string) int {
	fs := flag.NewFlagSet("journal", flag.ContinueOnError)
	session := fs.String("session", "", "filter by session id")
	agent := fs.String("agent", "", "filter by agent type")
	grep := fs.String("grep", "", "substring match on the summary")
	since := fs.String("since", "", "only entries at or after this RFC3339 time or Nd duration")
	export := fs.String("export", "", "render one session as markdown")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	repo, _, err := open()
	if err != nil {
		return fail(err)
	}
	root := repo.Root()

	if *export != "" {
		entries, _, err := journal.LoadSession(root, *export)
		if err != nil {
			return fail(err)
		}
		fmt.Print(journal.Export(*export, entries))
		return 0
	}

	var entries []journal.Entry
	var broken []journal.Broken
	if *session != "" {
		entries, broken, err = journal.LoadSession(root, *session)
	} else {
		entries, broken, err = journal.LoadAll(root)
	}
	if err != nil {
		return fail(err)
	}

	q := journal.Query{Session: *session, Agent: *agent, Grep: *grep}
	if *since != "" {
		t, err := parseSince(*since)
		if err != nil {
			return fail(err)
		}
		q.Since = t
	}
	matched := journal.Filter(entries, q)

	if *asJSON {
		emit(map[string]any{"entries": matched, "unreadable": len(broken)})
		return 0
	}
	for _, e := range matched {
		who := e.AgentType
		if who == "" {
			who = e.Event
		}
		fmt.Printf("%s  %s#%d  %-14s %s\n", e.TS, e.Session, e.Seq, who, oneLine(e.Summary))
	}
	for _, b := range broken {
		fmt.Fprintf(os.Stderr, "unreadable: %s line %d: %v\n", b.Path, b.Line, b.Err)
	}
	return 0
}

func parseSince(s string) (time.Time, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil {
			return time.Now().UTC().AddDate(0, 0, -days), nil
		}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: want RFC3339 or Nd", s)
	}
	return t, nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	return journal.Truncate(s, 120)
}

func cmdAck(args []string) int {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	none := fs.Bool("none", false, "record that a nudge was answered with no decision")
	session := fs.String("session", "", "session id (defaults to $CLAUDE_CODE_SESSION_ID)")
	drift := fs.String("drift", "", "not implemented (prd M4)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *drift != "" {
		fmt.Fprintln(os.Stderr, "dlog ack --drift: not built yet (prd §13, M4)")
		return 2
	}
	if !*none {
		fmt.Fprintln(os.Stderr, "dlog ack: need --none")
		return 2
	}
	repo, _, err := open()
	if err != nil {
		return fail(err)
	}
	sess := *session
	if sess == "" {
		sess = os.Getenv("CLAUDE_CODE_SESSION_ID")
	}
	if err := state.Append(repo.Root(), repo.WorktreeName(), state.Event{
		Kind: state.KindAck, Session: sess, How: state.AckNone,
	}); err != nil {
		return fail(err)
	}
	if *asJSON {
		emit(map[string]any{"acked": true, "session": sess, "how": state.AckNone})
	} else {
		fmt.Println("noted: no decision recorded for this nudge")
	}
	return 0
}

// cmdDrift is the M2 slice: the nudge response rate only. Stale, orphaned and
// unresolved findings are M4 (prd §11, §13), and reporting an empty list for them
// would read as "nothing is drifting" rather than "nothing was checked".
func cmdDrift(args []string) int {
	fs := flag.NewFlagSet("drift", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	repo, _, err := open()
	if err != nil {
		return fail(err)
	}
	events, err := state.LoadAll(repo.Root())
	if err != nil {
		return fail(err)
	}
	rate := state.ResponseRate(events, time.Now().UTC())

	if *asJSON {
		emit(map[string]any{
			"nudge_response": rate,
			"not_checked":    []string{"stale", "orphaned", "unresolved"},
		})
		return 0
	}
	fmt.Printf("nudge response rate: %.2f (%d answered of %d over %s)\n",
		rate.Rate, rate.Answered, rate.Nudges, rate.Window)
	if rate.Finding != "" {
		fmt.Println("finding:", rate.Finding)
	}
	fmt.Println("not checked: stale, orphaned, unresolved (prd §13, M4)")
	return 0
}

func brokenPaths(b []adr.Broken) []string {
	var out []string
	for _, x := range b {
		out = append(out, x.Path)
	}
	return out
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
