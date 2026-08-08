package adr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Marshal renders a brand-new record. Field order follows prd §6.1, and the list
// shapes match its example: affects as a block sequence, links inline.
//
// This is only ever used for files dlog itself creates. Existing files are edited
// surgically (see SetList and SetScalar) so that a key this version does not know
// about cannot be lost to a parse-and-reserialise round trip.
func (r *Record) Marshal() []byte {
	var b strings.Builder
	b.WriteString("---\n")
	scalar := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, quoteIfNeeded(v))
		}
	}
	scalar("id", r.ID)
	scalar("title", r.Title)
	scalar("status", r.Status)
	scalar("date", r.Date)
	scalar("author", r.Author)
	scalar("branch", r.Branch)
	scalar("worktree", r.Worktree)
	scalar("session", r.Session)

	if len(r.Affects) == 0 {
		b.WriteString("affects: []\n")
	} else {
		b.WriteString("affects:\n")
		for _, a := range r.Affects {
			fmt.Fprintf(&b, "  - %s\n", quoteIfNeeded(a))
		}
	}
	for _, f := range []struct {
		key  string
		vals []string
	}{
		{"supersedes", r.Supersedes},
		{"superseded_by", r.SupersededBy},
		{"depends_on", r.DependsOn},
		{"journal_refs", r.JournalRefs},
		{"tags", r.Tags},
	} {
		fmt.Fprintf(&b, "%s: %s\n", f.key, renderInline(f.vals))
	}
	scalar("drift_ack", r.DriftAck)
	b.WriteString("---\n")

	body := strings.TrimLeft(r.Body, "\n")
	if body == "" {
		body = Template()
	}
	b.WriteString("\n" + body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// Template is the empty body. The headings are fixed and lint checks they exist;
// what goes under them is the author's problem, and an honestly empty
// Alternatives section is better than a fabricated one (prd §4).
func Template() string {
	var b strings.Builder
	for i, s := range Sections {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "## %s\n\n", s)
	}
	return b.String()
}

// CheckScalar rejects values that cannot survive a single frontmatter line.
//
// A newline is the dangerous one: the tail of the value becomes its own line, so
// a title of "Harmless\nauthor:injected" writes a record whose author was never
// supplied and whose title is silently missing everything after the break. It
// lints clean, because on disk it is a perfectly well-formed record — of a
// different decision than the one that was asked for.
func CheckScalar(field, v string) error {
	for _, r := range v {
		if r == '\n' || r == '\r' {
			return fmt.Errorf("%s contains a line break; it must fit on one line", field)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a control character (%q)", field, r)
		}
	}
	return nil
}

func quoteIfNeeded(v string) string {
	if v == "" {
		return `""`
	}
	// Quote anything the scalar reader would otherwise mangle or reject.
	if strings.ContainsAny(v[:1], `|>&*{['"`) || strings.Contains(v, " #") || strings.Contains(v, ": ") {
		return `"` + strings.ReplaceAll(v, `"`, `'`) + `"`
	}
	return v
}

func renderInline(vals []string) string {
	if len(vals) == 0 {
		return "[]"
	}
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = quoteIfNeeded(v)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// Filename is the record's path relative to the shared root. The stem equals the
// id, and lint enforces that they stay in step.
func Filename(id string) string { return filepath.Join(Dir, id+".md") }

// Write creates a record, refusing to clobber an existing file.
func (r *Record) Write(root string) error {
	path := filepath.Join(root, Filename(r.ID))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	r.Path = path
	_, err = f.Write(r.Marshal())
	return err
}

// SetList rewrites one frontmatter list field in place, leaving every other byte
// of the file untouched. Surgical rather than reserialising the whole record:
// this edits a file another session may be holding open, and a round trip
// through the parser would silently drop anything the parser does not model.
func SetList(path, key string, vals []string) error {
	return edit(path, key, fmt.Sprintf("%s: %s", key, renderInline(vals)))
}

// SetScalar rewrites one frontmatter scalar field in place.
func SetScalar(path, key, val string) error {
	return edit(path, key, fmt.Sprintf("%s: %s", key, quoteIfNeeded(val)))
}

func edit(path, key, replacement string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	// Preserve the file's line endings. Normalising to LF and writing back would
	// rewrite every line of a CRLF file, which is the opposite of surgical and
	// turns a one-field edit into a whole-file diff.
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fmt.Errorf("%s: missing opening --- frontmatter delimiter", path)
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fmt.Errorf("%s: missing closing --- frontmatter delimiter", path)
	}

	prefix := key + ":"
	at := -1
	for i := 1; i < end; i++ {
		if lines[i] == prefix || strings.HasPrefix(lines[i], prefix+" ") {
			at = i
			break
		}
	}

	var out []string
	if at < 0 {
		// Absent: append at the end of the frontmatter block.
		out = append(out, lines[:end]...)
		out = append(out, replacement)
		out = append(out, lines[end:]...)
	} else {
		out = append(out, lines[:at]...)
		out = append(out, replacement)
		out = append(out, lines[blockEnd(lines, at+1, end):]...)
	}
	return os.WriteFile(path, []byte(strings.Join(out, newline)), 0o644)
}

// blockEnd returns the first line after the block sequence starting at from.
//
// Parse keeps collecting items across blank and comment lines, so the editor has
// to consume exactly the same span. Stopping at the first non-item line left the
// remaining items orphaned under a key that no longer opens a sequence — which
// made the target unparseable, on a code path whose whole job is not to break it.
//
// Blanks and comments are only consumed when another item follows, so a comment
// sitting between the list and the next key survives. A comment interleaved
// between two items does not; that is a real if small loss, and the alternative
// is reconstructing where it belonged in a list that just changed length.
func blockEnd(lines []string, from, end int) int {
	drop := from
	for {
		j := drop
		for j < end && isBlankOrComment(lines[j]) {
			j++
		}
		if j < end && isSeqItem(lines[j]) {
			drop = j + 1
			continue
		}
		return drop
	}
}

func isBlankOrComment(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

func isSeqItem(line string) bool {
	t := strings.TrimSpace(line)
	return t == "-" || strings.HasPrefix(t, "- ")
}
