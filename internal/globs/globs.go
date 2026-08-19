// Package globs matches `affects` patterns against repo-relative paths.
//
// path.Match has no `**`, and `**` is the whole point of an affects glob
// (`internal/pricing/**`), so the segment walk below is hand-rolled rather than
// delegated. Three callers depend on it — lint's rot check, journal_refs
// inference, and the retrieval path overlap — so it lives in one place.
package globs

import (
	"path"
	"strings"
)

// Match reports whether a repo-relative path matches a glob pattern.
//
// Supported: `*` and `?` within a segment (via path.Match), and `**` spanning
// any number of segments including zero. Patterns and paths are compared with
// forward slashes; a leading "./" on either side is ignored.
func Match(pattern, name string) bool {
	// Before Clean, not after: path.Clean("") is ".", so checking afterwards never
	// fires and an empty pattern matches things.
	if pattern == "" || name == "" {
		return false
	}
	pattern = strings.TrimPrefix(path.Clean(pattern), "./")
	name = strings.TrimPrefix(path.Clean(name), "./")
	// A bare directory prefix is treated as covering the directory's contents, so
	// `internal/pricing` behaves like `internal/pricing/**`. Writing the trailing
	// `/**` is easy to forget and the surprise is silent (an ADR matching nothing).
	if !strings.ContainsAny(pattern, "*?") && (name == pattern || strings.HasPrefix(name, pattern+"/")) {
		return true
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// `**` is allowed to consume zero segments, so a trailing `**` matches the
			// directory itself as well as everything under it.
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(seg); i++ {
				if matchSegments(pat[1:], seg[i:]) {
					return true
				}
			}
			return false
		}
		if len(seg) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], seg[0]); err != nil || !ok {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	return len(seg) == 0
}

// MatchAny reports whether name matches any of the patterns.
func MatchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if Match(p, name) {
			return true
		}
	}
	return false
}

// Overlap returns the names matched by at least one pattern, preserving order.
func Overlap(patterns, names []string) []string {
	var out []string
	for _, n := range names {
		if MatchAny(patterns, n) {
			out = append(out, n)
		}
	}
	return out
}
