package adr

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/GuitarWag/adrlog/internal/globs"
)

// Levels. Only Error fails the command; a Warning is addressed to the human
// reading the output, never a gate the agent has to clear.
const (
	Error   = "error"
	Warning = "warning"
)

type Finding struct {
	Level string `json:"level"`
	Path  string `json:"path"`
	ID    string `json:"id,omitempty"`
	Msg   string `json:"msg"`
}

// Options supplies the facts lint cannot derive from the records alone.
type Options struct {
	// Tracked is the repository's tracked file list, for the affects rot check.
	// Nil disables that check rather than reporting every glob as orphaned.
	Tracked []string
	// RefExists resolves a journal_refs pointer. Nil disables the check. Lint only
	// ever asks whether a pointer resolves, never whether it is apt.
	RefExists func(ref string) bool
	// KnownSession reports whether a session's journal is present at all. Nil
	// treats every session as absent, which downgrades the checks below rather
	// than failing them.
	//
	// This distinction is the difference between "the pointer is wrong" and "the
	// journal is not here". A journal is legitimately absent on any machine but
	// the one that wrote it when `journal_committed` is false, and on
	// every machine once `prune` drops it at 90 days. Treating that as a
	// failure turns an advisory field into a build break that nobody can fix.
	KnownSession func(session string) bool
}

var journalRef = regexp.MustCompile(`^[^#\s]+#\d+$`)

// Lint checks the record set. Findings come back sorted, errors first.
func Lint(recs []*Record, broken []Broken, opt Options) []Finding {
	var f []Finding
	add := func(level, path, id, format string, args ...any) {
		f = append(f, Finding{level, path, id, fmt.Sprintf(format, args...)})
	}

	// A record the parser cannot read is a broken record, not an invisible one.
	// This is the highest-value rule in the file, because everything else
	// assumes the record was seen at all.
	for _, b := range broken {
		add(Error, b.Path, "", "unreadable record: %s", b.Err)
	}

	byID := map[string]*Record{}
	seen := map[string]string{}
	for _, r := range recs {
		if r.ID == "" {
			continue
		}
		if prev, dup := seen[r.ID]; dup {
			add(Error, r.Path, r.ID, "duplicate id, also in %s", filepath.Base(prev))
			continue
		}
		seen[r.ID] = r.Path
		byID[r.ID] = r
	}

	for _, r := range recs {
		for _, req := range []struct{ name, val string }{
			{"id", r.ID}, {"title", r.Title}, {"status", r.Status}, {"date", r.Date},
		} {
			if req.val == "" {
				add(Error, r.Path, r.ID, "missing required field %q", req.name)
			}
		}

		if r.Status != "" && !contains(Statuses, r.Status) {
			add(Error, r.Path, r.ID, "invalid status %q, want one of %s", r.Status, strings.Join(Statuses, ", "))
		}
		if r.Date != "" {
			if _, err := time.Parse(DateFormat, r.Date); err != nil {
				add(Error, r.Path, r.ID, "invalid date %q, want YYYY-MM-DD", r.Date)
			}
		}
		if stem := strings.TrimSuffix(filepath.Base(r.Path), ".md"); r.ID != "" && stem != r.ID {
			add(Error, r.Path, r.ID, "filename stem %q does not match id %q", stem, r.ID)
		}

		// Link integrity. A dangling pointer makes the graph lie, and the graph is
		// the thing that is supposed to be queryable rather than prose.
		for _, l := range []struct {
			key  string
			vals []string
		}{
			{"supersedes", r.Supersedes},
			{"superseded_by", r.SupersededBy},
			{"depends_on", r.DependsOn},
		} {
			for _, id := range l.vals {
				if _, ok := byID[id]; !ok {
					add(Error, r.Path, r.ID, "%s points at unknown record %q", l.key, id)
				}
			}
		}

		// Reciprocity, both directions. A back-link lost to a race with
		// another session shows up here rather than staying silent.
		for _, id := range r.Supersedes {
			if t, ok := byID[id]; ok && !contains(t.SupersededBy, r.ID) {
				add(Error, t.Path, t.ID, "missing reciprocal superseded_by %q (superseded by that record)", r.ID)
			}
		}
		for _, id := range r.SupersededBy {
			if t, ok := byID[id]; ok && !contains(t.Supersedes, r.ID) {
				add(Error, t.Path, t.ID, "missing reciprocal supersedes %q", r.ID)
			}
		}
		if len(r.SupersededBy) > 0 && r.Status != "superseded" {
			add(Error, r.Path, r.ID, "has superseded_by but status is %q, want \"superseded\"", r.Status)
		}

		for _, ref := range r.JournalRefs {
			// A malformed pointer is always an error: it is wrong on its face, and no
			// journal anywhere could make `session#notanumber` resolve.
			if !journalRef.MatchString(ref) {
				add(Error, r.Path, r.ID, "malformed journal_ref %q, want session#seq", ref)
				continue
			}
			if opt.RefExists == nil || opt.RefExists(ref) {
				continue
			}
			session, _, _ := strings.Cut(ref, "#")
			if opt.KnownSession != nil && opt.KnownSession(session) {
				// The journal is here and does not contain that turn, so the pointer is
				// genuinely wrong.
				add(Error, r.Path, r.ID, "journal_ref %q names a turn its session's journal does not have", ref)
			} else {
				add(Warning, r.Path, r.ID, "journal_ref %q cannot be checked, session %q has no journal here", ref, session)
			}
		}

		for _, s := range Sections {
			if !r.HasSection(s) {
				add(Error, r.Path, r.ID, "missing body section %q", s)
			}
		}

		// Warnings from here down. None of these fail the command.

		// Empty Alternatives on an accepted record is deliberately a warning to the
		// human, never an error: failing on it would train the agent to invent
		// rejected options, and fabricated alternatives are the single worst thing
		// this log can contain.
		if r.Status == "accepted" && r.HasSection("Alternatives considered") && r.Section("Alternatives considered") == "" {
			add(Warning, r.Path, r.ID, "accepted record has an empty Alternatives section (for a human to judge, not to pad)")
		}

		// Rot check: a glob matching nothing means the code moved or died.
		// MatchAny, not Overlap: this is a boolean question, and Overlap builds the
		// whole match list without early exit — at a few hundred records over a few
		// thousand tracked files that was most of lint's runtime, and lint runs
		// inside a hook with a 50ms budget.
		if opt.Tracked != nil {
			for _, g := range r.Affects {
				if !anyMatch(g, opt.Tracked) {
					add(Warning, r.Path, r.ID, "affects glob %q matches no tracked file", g)
				}
			}
		}

		for _, k := range r.Unknown {
			add(Warning, r.Path, r.ID, "unknown frontmatter key %s, left untouched", k)
		}
	}

	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Level != f[j].Level {
			return f[i].Level == Error
		}
		return f[i].Path < f[j].Path
	})
	return f
}

// HasErrors reports whether any finding fails the command.
func HasErrors(f []Finding) bool {
	for _, x := range f {
		if x.Level == Error {
			return true
		}
	}
	return false
}

func anyMatch(pattern string, names []string) bool {
	for _, n := range names {
		if globs.Match(pattern, n) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
