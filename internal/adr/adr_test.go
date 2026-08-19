package adr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const good = `---
id: 20260804-143210-store-list-price-and-effective-price
title: Store list price and effective price as separate fields
status: accepted          # proposed | accepted | rejected | superseded | reverted
date: 2026-08-04
author: subagent:schema-reviewer
affects:
  - internal/pricing/**
supersedes: []
superseded_by: []
depends_on: [20260801-091500-append-only-event-log]
journal_refs: [4f2a91c#12, 4f2a91c#15]
tags: [pricing, billing]
---

## Context

Pricing rules changed.

## Decision

Two fields.

## Alternatives considered

One field with a flag.

## Consequences

Migration needed.
`

func TestParseSupportedSubset(t *testing.T) {
	r, err := Parse("x.md", []byte(good))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.ID != "20260804-143210-store-list-price-and-effective-price" {
		t.Errorf("id = %q", r.ID)
	}
	// A trailing comment must not end up inside the value. Real frontmatter carries
	// one on the status line.
	if r.Status != "accepted" {
		t.Errorf("status = %q, comment not stripped", r.Status)
	}
	if len(r.Affects) != 1 || r.Affects[0] != "internal/pricing/**" {
		t.Errorf("affects = %v (block sequence)", r.Affects)
	}
	if len(r.Supersedes) != 0 {
		t.Errorf("supersedes = %v, want empty for []", r.Supersedes)
	}
	if len(r.JournalRefs) != 2 || r.JournalRefs[1] != "4f2a91c#15" {
		t.Errorf("journal_refs = %v (inline array)", r.JournalRefs)
	}
	if r.Section("Decision") != "Two fields." {
		t.Errorf("Decision section = %q", r.Section("Decision"))
	}
	if r.Section("Context") != "Pricing rules changed." {
		t.Errorf("Context section = %q", r.Section("Context"))
	}
}

// Unsupported YAML must fail loudly with a line number. This is the hazard the
// hand-rolled parser creates: a record reflowed into legal YAML this
// subset does not implement would otherwise vanish from every index and query
// while looking perfectly fine on disk.
func TestParseRejectsUnsupportedYAMLWithLineNumbers(t *testing.T) {
	cases := []struct {
		name, frontmatter string
		wantLine          int
	}{
		// Line 1 is the opening ---, line 2 is `id: a`, so the offender is line 3
		// except where the construct only becomes illegal on its continuation line.
		{"folded scalar", "id: a\ntitle: >\n  folded text\n", 3},
		{"literal scalar", "id: a\ntitle: |\n  literal text\n", 3},
		{"anchor", "id: a\ntags: &anchor [x]\n", 3},
		{"alias", "id: a\ntags: *anchor\n", 3},
		{"flow mapping", "id: a\nmeta: {x: 1}\n", 3},
		{"nested mapping", "id: a\nmeta:\n  nested: 1\n", 4},
		{"tab indent", "id: a\naffects:\n\t- x\n", 4},
		{"unterminated inline array", "id: a\ntags: [x, y\n", 3},
		{"unterminated quote", "id: a\ntitle: \"unclosed\n", 3},
		{"junk line", "id: a\nthis is not yaml\n", 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse("x.md", []byte("---\n"+c.frontmatter+"---\n\nbody\n"))
			if err == nil {
				t.Fatal("expected a parse error, got none — the record would be silently skipped")
			}
			pe, ok := err.(*ParseError)
			if !ok {
				t.Fatalf("error is %T, want *ParseError with a line number", err)
			}
			if pe.Line != c.wantLine {
				t.Errorf("reported line %d, want %d (%v)", pe.Line, c.wantLine, err)
			}
		})
	}
}

func TestParseMissingDelimiters(t *testing.T) {
	if _, err := Parse("x.md", []byte("no frontmatter\n")); err == nil {
		t.Error("expected an error for a file with no frontmatter")
	}
	if _, err := Parse("x.md", []byte("---\nid: a\nnever closed\n")); err == nil {
		t.Error("expected an error for unterminated frontmatter")
	}
}

func TestSlugAndID(t *testing.T) {
	// Six words, lowercased.
	if got := Slug("Store list price and effective price as separate fields"); got != "store-list-price-and-effective-price" {
		t.Errorf("Slug = %q", got)
	}
	if got := Slug("Use Postgres!"); got != "use-postgres" {
		t.Errorf("Slug = %q", got)
	}
	if got := Slug("!!! ???"); got != "untitled" {
		t.Errorf("Slug on punctuation-only = %q", got)
	}
	when := time.Date(2026, 8, 4, 14, 32, 10, 0, time.UTC)
	if got := NewID(when, "Use Postgres"); got != "20260804-143210-use-postgres" {
		t.Errorf("NewID = %q", got)
	}
}

func TestLintCatchesStructuralDefects(t *testing.T) {
	mk := func(path string, body string) *Record {
		r, err := Parse(path, []byte(body))
		if err != nil {
			t.Fatalf("fixture %s: %v", path, err)
		}
		return r
	}
	sections := "\n## Context\nc\n## Decision\nd\n## Alternatives considered\na\n## Consequences\nq\n"

	recs := []*Record{
		// Filename stem out of step with the id.
		mk("/r/docs/adr/wrong-name.md", "---\nid: 20260101-000000-a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\n---\n"+sections),
		// Claims to supersede a record that never learned about it.
		mk("/r/docs/adr/20260102-000000-b.md", "---\nid: 20260102-000000-b\ntitle: B\nstatus: accepted\ndate: 2026-01-02\nsupersedes: [20260101-000000-a]\n---\n"+sections),
		// Points at a record that does not exist.
		mk("/r/docs/adr/20260103-000000-c.md", "---\nid: 20260103-000000-c\ntitle: C\nstatus: bogus\ndate: nonsense\ndepends_on: [nope]\n---\n"+sections),
	}
	f := Lint(recs, []Broken{{Path: "/r/docs/adr/oops.md", Err: &ParseError{Path: "oops.md", Line: 3, Msg: "bad"}}}, Options{})

	want := []string{
		"unreadable record",
		"filename stem",
		"missing reciprocal superseded_by",
		"invalid status",
		"invalid date",
		"depends_on points at unknown record",
	}
	joined := renderFindings(f)
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("missing finding %q in:\n%s", w, joined)
		}
	}
	if !HasErrors(f) {
		t.Error("expected errors")
	}
}

// An accepted record with an empty Alternatives section is a warning to a human,
// never an error. Failing on it would train the agent to fabricate rejected
// options, and fabricated alternatives are the worst thing this log can hold.
// This test exists to stop a future tightening of the rule.
func TestEmptyAlternativesIsWarningNotError(t *testing.T) {
	body := "---\nid: 20260101-000000-a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\n---\n\n## Context\nc\n## Decision\nd\n## Alternatives considered\n\n## Consequences\nq\n"
	r, err := Parse("/r/docs/adr/20260101-000000-a.md", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	f := Lint([]*Record{r}, nil, Options{})
	if HasErrors(f) {
		t.Fatalf("empty Alternatives must not fail lint:\n%s", renderFindings(f))
	}
	if !strings.Contains(renderFindings(f), "empty Alternatives") {
		t.Errorf("expected a warning, got:\n%s", renderFindings(f))
	}
}

// A glob matching nothing means the code moved or died. This is the rot check.
func TestLintAffectsRotCheck(t *testing.T) {
	body := "---\nid: 20260101-000000-a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\naffects:\n  - internal/gone/**\n  - internal/here/**\n---\n\n## Context\nc\n## Decision\nd\n## Alternatives considered\na\n## Consequences\nq\n"
	r, _ := Parse("/r/docs/adr/20260101-000000-a.md", []byte(body))
	f := Lint([]*Record{r}, nil, Options{Tracked: []string{"internal/here/x.go"}})
	out := renderFindings(f)
	if !strings.Contains(out, `affects glob "internal/gone/**" matches no tracked file`) {
		t.Errorf("expected rot finding, got:\n%s", out)
	}
	if strings.Contains(out, "internal/here") {
		t.Errorf("live glob should not be flagged:\n%s", out)
	}
	if HasErrors(f) {
		t.Error("rot is a warning, not an error")
	}
}

func TestLintJournalRefs(t *testing.T) {
	body := "---\nid: 20260101-000000-a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\njournal_refs: [abc#1, malformed, abc#99]\n---\n\n## Context\nc\n## Decision\nd\n## Alternatives considered\na\n## Consequences\nq\n"
	r, _ := Parse("/r/docs/adr/20260101-000000-a.md", []byte(body))
	f := Lint([]*Record{r}, nil, Options{
		RefExists:    func(ref string) bool { return ref == "abc#1" },
		KnownSession: func(s string) bool { return s == "abc" },
	})
	out := renderFindings(f)
	if !strings.Contains(out, `malformed journal_ref "malformed"`) {
		t.Errorf("expected malformed ref finding:\n%s", out)
	}
	// The journal for "abc" is present and has no turn 99, so the pointer is wrong.
	if !strings.Contains(out, `journal_ref "abc#99" names a turn`) {
		t.Errorf("expected an error for a wrong ref into a present journal:\n%s", out)
	}
	if !HasErrors(f) {
		t.Error("a wrong ref into a present journal should fail lint")
	}
}

// A journal is legitimately absent on any machine but the one that wrote it when
// journal_committed is false, and on every machine once prune drops it
// at 90 days. Treating that as an error turned an advisory field into a
// build break nobody could fix: CI checked out this very repo, found no journal,
// and failed on ten pointers that were perfectly correct.
func TestAbsentJournalDowngradesRefCheck(t *testing.T) {
	body := "---\nid: 20260101-000000-a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\njournal_refs: [gone#1, gone#2]\n---\n\n## Context\nc\n## Decision\nd\n## Alternatives considered\na\n## Consequences\nq\n"
	r, _ := Parse("/r/docs/adr/20260101-000000-a.md", []byte(body))
	f := Lint([]*Record{r}, nil, Options{
		RefExists:    func(string) bool { return false },
		KnownSession: func(string) bool { return false },
	})
	if HasErrors(f) {
		t.Fatalf("an absent journal must not fail lint:\n%s", renderFindings(f))
	}
	if !strings.Contains(renderFindings(f), "cannot be checked") {
		t.Errorf("expected a warning naming the absent session:\n%s", renderFindings(f))
	}
}

// Round-tripping a record through parse-and-reserialise would drop any key this
// version does not model. Edits are surgical precisely so that cannot happen.
func TestSurgicalEditPreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\nid: 20260101-000000-a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\nfuture_field: keep me\nsuperseded_by: []\naffects:\n  - internal/**\n---\n\n## Context\nkeep this body\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetList(path, "superseded_by", []string{"20260202-000000-b"}); err != nil {
		t.Fatal(err)
	}
	if err := SetScalar(path, "status", "superseded"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	got := string(out)
	for _, want := range []string{
		"future_field: keep me",
		"superseded_by: [20260202-000000-b]",
		"status: superseded",
		"  - internal/**",
		"keep this body",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q after edit:\n%s", want, got)
		}
	}
	if strings.Count(got, "status:") != 1 {
		t.Errorf("status duplicated:\n%s", got)
	}
}

// Adding a key that was absent must land inside the frontmatter, not after the
// closing delimiter where it would become body text.
func TestSurgicalEditInsertsMissingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	os.WriteFile(path, []byte("---\nid: a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\n---\n\n## Context\nc\n"), 0o644)
	if err := SetList(path, "superseded_by", []string{"b"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	r, err := Parse(path, data)
	if err != nil {
		t.Fatalf("record no longer parses after insert: %v\n%s", err, data)
	}
	if len(r.SupersededBy) != 1 || r.SupersededBy[0] != "b" {
		t.Errorf("superseded_by = %v", r.SupersededBy)
	}
}

// Parse keeps collecting list items across blank and comment lines, so the editor
// has to consume the same span. Stopping at the first non-item line left the rest
// orphaned under a key that no longer opened a sequence, which made the target
// unreadable — a --supersedes that destroys the record it points at.
func TestSurgicalEditConsumesWholeBlockSequence(t *testing.T) {
	cases := map[string]string{
		"comment between items": "superseded_by:\n  - ghost-a\n  # a human note\n  - ghost-b\n",
		"blank between items":   "superseded_by:\n  - ghost-a\n\n  - ghost-b\n",
		"bare dash item":        "superseded_by:\n  - ghost-a\n  -\n  - ghost-b\n",
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.md")
			body := "---\nid: a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\n" + block + "tags: [x]\n---\n\n## Context\nc\n"
			if _, err := Parse(path, []byte(body)); err != nil {
				t.Fatalf("fixture does not parse before the edit: %v", err)
			}
			os.WriteFile(path, []byte(body), 0o644)

			if err := SetList(path, "superseded_by", []string{"new-id"}); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			r, err := Parse(path, data)
			if err != nil {
				t.Fatalf("record unreadable after edit: %v\n%s", err, data)
			}
			if len(r.SupersededBy) != 1 || r.SupersededBy[0] != "new-id" {
				t.Errorf("superseded_by = %v\n%s", r.SupersededBy, data)
			}
			if strings.Contains(string(data), "ghost-b") {
				t.Errorf("orphaned list item left behind:\n%s", data)
			}
			if len(r.Tags) != 1 || r.Tags[0] != "x" {
				t.Errorf("edit ate the following key: tags = %v", r.Tags)
			}
		})
	}
}

// A comment that follows the list rather than sitting inside it is not part of
// the list and must survive.
func TestSurgicalEditKeepsTrailingComment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	os.WriteFile(path, []byte("---\nid: a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\nsuperseded_by:\n  - old\n\n# unrelated note\ntags: [x]\n---\n\n## Context\nc\n"), 0o644)
	if err := SetList(path, "superseded_by", []string{"new"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# unrelated note") {
		t.Errorf("trailing comment was eaten:\n%s", data)
	}
	if _, err := Parse(path, data); err != nil {
		t.Fatalf("unreadable after edit: %v\n%s", err, data)
	}
}

// "Surgical" has to mean it. Normalising line endings rewrote every line of a
// CRLF file, turning a one-field edit into a whole-file diff.
func TestSurgicalEditPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	body := strings.ReplaceAll("---\nid: a\ntitle: A\nstatus: accepted\ndate: 2026-01-01\nsuperseded_by: []\n---\n\n## Context\nc\n", "\n", "\r\n")
	os.WriteFile(path, []byte(body), 0o644)
	before := strings.Count(body, "\r\n")

	if err := SetList(path, "superseded_by", []string{"new"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if after := strings.Count(string(data), "\r\n"); after != before {
		t.Errorf("CRLF count %d -> %d; the whole file was rewritten", before, after)
	}
	if _, err := Parse(path, data); err != nil {
		t.Fatalf("unreadable after edit: %v", err)
	}
}

// A newline in a title wrote extra frontmatter fields and dropped the rest of the
// title, producing a record that lints clean and describes something else.
func TestCheckScalarRejectsLineBreaksAndControls(t *testing.T) {
	if err := CheckScalar("title", "Harmless title\nauthor:injected\ndrift_ack:2099-01-01"); err == nil {
		t.Error("a newline in a title must be rejected")
	}
	if err := CheckScalar("title", "carriage\rreturn"); err == nil {
		t.Error("a carriage return in a title must be rejected")
	}
	if err := CheckScalar("title", "bell\x07"); err == nil {
		t.Error("a control character in a title must be rejected")
	}
	if err := CheckScalar("title", "Perfectly ordinary: a title, with punctuation — and unicode ✓"); err != nil {
		t.Errorf("rejected a legitimate title: %v", err)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	r := &Record{
		ID: "20260101-000000-a", Title: "Use: a title with a colon", Status: "accepted",
		Date: "2026-01-01", Affects: []string{"internal/**"}, Tags: []string{"x", "y"},
	}
	got, err := Parse("a.md", r.Marshal())
	if err != nil {
		t.Fatalf("own output does not parse: %v\n%s", err, r.Marshal())
	}
	if got.Title != r.Title {
		t.Errorf("title = %q, want %q", got.Title, r.Title)
	}
	for _, s := range Sections {
		if !got.HasSection(s) {
			t.Errorf("template is missing section %q", s)
		}
	}
}

func TestIndexIsDeterministic(t *testing.T) {
	recs := []*Record{
		{ID: "20260102-000000-b", Title: "B", Status: "accepted", Date: "2026-01-02", Supersedes: []string{"20260101-000000-a"}},
		{ID: "20260101-000000-a", Title: "A", Status: "superseded", Date: "2026-01-01"},
	}
	first := string(Index(recs))
	reversed := []*Record{recs[1], recs[0]}
	// Concurrent regeneration is only safe if input order cannot change the bytes
	//: last-write-wins has to mean same-write-wins.
	if second := string(Index(reversed)); first != second {
		t.Error("index output depends on input order; concurrent writes would fight")
	}
	if !strings.Contains(first, "supersedes") || !strings.Contains(first, "mermaid") {
		t.Errorf("index missing graph:\n%s", first)
	}
}

func renderFindings(f []Finding) string {
	var b strings.Builder
	for _, x := range f {
		b.WriteString(x.Level + " " + x.Path + ": " + x.Msg + "\n")
	}
	return b.String()
}
