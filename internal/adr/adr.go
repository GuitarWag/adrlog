// Package adr parses, writes, lints and indexes decision records.
//
// The frontmatter parser is hand-rolled against a deliberately small YAML subset
// — scalars, inline arrays, block sequences — so the binary carries no YAML
// dependency and the format stays trivial to reimplement (prd §6.1). The hazard
// that creates is the whole reason Parse reports line numbers: an editor
// reflowing frontmatter into legal-but-unsupported YAML must produce a loud lint
// failure, never a record that quietly vanishes from every index and query.
package adr

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Dir is where records live, relative to the shared root.
const Dir = "docs/adr"

// DateFormat is the frontmatter date layout.
const DateFormat = "2006-01-02"

// Statuses are the legal values of the status field (prd §6.1).
var Statuses = []string{"proposed", "accepted", "rejected", "superseded", "reverted"}

// Sections are the fixed body headings checked by lint (prd §6.1).
var Sections = []string{"Context", "Decision", "Alternatives considered", "Consequences"}

// Record is one decision record.
type Record struct {
	ID           string
	Title        string
	Status       string
	Date         string
	Author       string
	Branch       string
	Worktree     string
	Session      string
	DriftAck     string
	Affects      []string
	Supersedes   []string
	SupersededBy []string
	DependsOn    []string
	JournalRefs  []string
	Tags         []string

	// Unknown frontmatter keys, kept so lint can name them rather than dropping
	// them on the floor. Nothing rewrites them away: edits to existing files are
	// surgical, so a key this version does not understand survives untouched.
	Unknown []string

	Path string // absolute path on disk
	Body string // everything after the closing ---
}

// ParseError carries the offending line, so an unsupported construct is reported
// precisely instead of as a vague "could not read record".
type ParseError struct {
	Path string
	Line int
	Msg  string
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.Path, e.Line, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Msg)
}

var (
	keyLine   = regexp.MustCompile(`^([a-z_][a-z0-9_]*):(.*)$`)
	slugSplit = regexp.MustCompile(`[^a-z0-9]+`)
)

// knownKeys maps a frontmatter key to whether it holds a list.
var knownKeys = map[string]bool{
	"id": false, "title": false, "status": false, "date": false,
	"author": false, "branch": false, "worktree": false, "session": false,
	"drift_ack": false,
	"affects":   true, "supersedes": true, "superseded_by": true,
	"depends_on": true, "journal_refs": true, "tags": true,
}

// Parse reads one record. Every failure names a line; none of them are silent.
func Parse(path string, data []byte) (*Record, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, &ParseError{Path: path, Line: 1, Msg: "missing opening --- frontmatter delimiter"}
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, &ParseError{Path: path, Line: len(lines), Msg: "missing closing --- frontmatter delimiter"}
	}

	r := &Record{Path: path, Body: strings.Join(lines[end+1:], "\n")}
	var curKey string // the list key currently accepting block-sequence items

	for i := 1; i < end; i++ {
		line := lines[i]
		num := i + 1
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.ContainsRune(line, '\t') {
			return nil, &ParseError{path, num, "tab in frontmatter; this parser accepts spaces only"}
		}

		// Block-sequence item, continuing the key above it.
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			if curKey == "" {
				return nil, &ParseError{path, num, "list item with no key above it"}
			}
			item, err := scalar(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")), path, num)
			if err != nil {
				return nil, err
			}
			if item != "" {
				r.appendList(curKey, item)
			}
			continue
		}

		if line != trimmed {
			return nil, &ParseError{path, num, "unexpected indentation; nested mappings are not supported"}
		}

		m := keyLine.FindStringSubmatch(line)
		if m == nil {
			return nil, &ParseError{path, num, fmt.Sprintf("not a supported frontmatter line: %q", trimmed)}
		}
		key, rest := m[1], strings.TrimSpace(m[2])
		isList, known := knownKeys[key]
		if !known {
			r.Unknown = append(r.Unknown, fmt.Sprintf("%s (line %d)", key, num))
		}
		curKey = ""

		switch {
		case rest == "":
			// A bare key opens a block sequence. An empty one stays empty.
			if !known || isList {
				curKey = key
				continue
			}
			r.setScalar(key, "")
		case strings.HasPrefix(rest, "["):
			items, err := inlineList(rest, path, num)
			if err != nil {
				return nil, err
			}
			for _, it := range items {
				r.appendList(key, it)
			}
		default:
			v, err := scalar(rest, path, num)
			if err != nil {
				return nil, err
			}
			r.setScalar(key, v)
		}
	}
	return r, nil
}

// scalar reads one value, rejecting the YAML constructs this subset does not
// implement rather than misreading them.
func scalar(s, path string, num int) (string, error) {
	if s == "" {
		return "", nil
	}
	switch s[0] {
	case '|', '>':
		return "", &ParseError{path, num, "block scalars (| and >) are not supported; use a single-line value"}
	case '&', '*':
		return "", &ParseError{path, num, "anchors and aliases are not supported"}
	case '{':
		return "", &ParseError{path, num, "flow mappings are not supported"}
	case '\'', '"':
		q := rune(s[0])
		closing := strings.IndexRune(s[1:], q)
		if closing < 0 {
			return "", &ParseError{path, num, "unterminated quoted value"}
		}
		v := s[1 : 1+closing]
		if after := strings.TrimSpace(s[2+closing:]); after != "" && !strings.HasPrefix(after, "#") {
			return "", &ParseError{path, num, "unexpected text after quoted value"}
		}
		return v, nil
	}
	// Strip a trailing comment. A value that needs a literal " #" must be quoted;
	// the prd's own examples carry inline comments, so supporting them wins.
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s), nil
}

func inlineList(s, path string, num int) ([]string, error) {
	closing := strings.LastIndex(s, "]")
	if closing < 0 {
		return nil, &ParseError{path, num, "unterminated inline array"}
	}
	if after := strings.TrimSpace(s[closing+1:]); after != "" && !strings.HasPrefix(after, "#") {
		return nil, &ParseError{path, num, "unexpected text after inline array"}
	}
	inner := strings.TrimSpace(s[1:closing])
	if inner == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(inner, ",") {
		v, err := scalar(strings.TrimSpace(part), path, num)
		if err != nil {
			return nil, err
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

func (r *Record) setScalar(key, v string) {
	switch key {
	case "id":
		r.ID = v
	case "title":
		r.Title = v
	case "status":
		r.Status = v
	case "date":
		r.Date = v
	case "author":
		r.Author = v
	case "branch":
		r.Branch = v
	case "worktree":
		r.Worktree = v
	case "session":
		r.Session = v
	case "drift_ack":
		r.DriftAck = v
	}
}

func (r *Record) appendList(key, v string) {
	switch key {
	case "affects":
		r.Affects = append(r.Affects, v)
	case "supersedes":
		r.Supersedes = append(r.Supersedes, v)
	case "superseded_by":
		r.SupersededBy = append(r.SupersededBy, v)
	case "depends_on":
		r.DependsOn = append(r.DependsOn, v)
	case "journal_refs":
		r.JournalRefs = append(r.JournalRefs, v)
	case "tags":
		r.Tags = append(r.Tags, v)
	}
}

// Section returns the body text under a `## <name>` heading, trimmed.
func (r *Record) Section(name string) string {
	lines := strings.Split(r.Body, "\n")
	start := -1
	for i, l := range lines {
		if strings.EqualFold(strings.TrimSpace(l), "## "+name) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	var out []string
	for _, l := range lines[start:] {
		if strings.HasPrefix(strings.TrimSpace(l), "## ") {
			break
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// HasSection reports whether the heading exists at all, which is distinct from
// it existing but being empty: one is a malformed record, the other is honest.
func (r *Record) HasSection(name string) bool {
	for _, l := range strings.Split(r.Body, "\n") {
		if strings.EqualFold(strings.TrimSpace(l), "## "+name) {
			return true
		}
	}
	return false
}

// Broken is a record on disk that could not be read. Lint reports these as
// failures; nothing else is allowed to skip them quietly (prd §6.1).
type Broken struct {
	Path string
	Err  error
}

// LoadAll reads every record under docs/adr/, returning the ones that parsed and
// the ones that did not. README.md is the generated index, not a record.
func LoadAll(root string) ([]*Record, []Broken, error) {
	dir := filepath.Join(root, Dir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var recs []*Record
	var broken []Broken
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "README.md" {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			broken = append(broken, Broken{path, err})
			continue
		}
		rec, err := Parse(path, data)
		if err != nil {
			broken = append(broken, Broken{path, err})
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	sort.Slice(broken, func(i, j int) bool { return broken[i].Path < broken[j].Path })
	return recs, broken, nil
}

// Slug is the first six words of the title, lowercased (prd §5.1).
func Slug(title string) string {
	words := slugSplit.Split(strings.ToLower(title), -1)
	var kept []string
	for _, w := range words {
		if w == "" {
			continue
		}
		kept = append(kept, w)
		if len(kept) == 6 {
			break
		}
	}
	if len(kept) == 0 {
		return "untitled"
	}
	return strings.Join(kept, "-")
}

// idTimeFormat is the timestamp half of an id.
const idTimeFormat = "20060102-150405"

// NewID builds YYYYMMDD-HHMMSS-slug (prd §5.1). Sortable, readable, and needing
// no coordination between worktrees — which is the entire point, since a
// sequential counter is what collides across five parallel sessions.
func NewID(t time.Time, title string) string {
	return t.Format(idTimeFormat) + "-" + Slug(title)
}

// Created reads the creation time back out of the id. Local time, matching how
// NewID wrote it. False when the id does not carry one, which includes every
// hand-named file.
func (r *Record) Created() (time.Time, bool) {
	if len(r.ID) < len(idTimeFormat) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(idTimeFormat, r.ID[:len(idTimeFormat)], time.Local)
	return t, err == nil
}
