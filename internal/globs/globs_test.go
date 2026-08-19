package globs

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		// `**` spans any number of segments, including zero. path.Match cannot do
		// this at all, which is why the matcher is hand-rolled.
		{"internal/pricing/**", "internal/pricing/store.go", true},
		{"internal/pricing/**", "internal/pricing/sub/deep/store.go", true},
		{"internal/pricing/**", "internal/pricing", true},
		{"internal/pricing/**", "internal/other/store.go", false},
		{"**/*_test.go", "a/b/c_test.go", true},
		{"**/*_test.go", "c_test.go", true},
		{"**/*.tf", "infra/main.tf", true},
		{"**", "anything/at/all.go", true},

		// Single-segment wildcards stay within their segment.
		{"migrations/*_balances.sql", "migrations/003_balances.sql", true},
		{"migrations/*_balances.sql", "migrations/sub/003_balances.sql", false},
		{"cmd/*", "cmd/adrlog", true},
		{"cmd/*", "cmd/adrlog/main.go", false},

		// A bare directory prefix covers its contents; forgetting the `/**` is easy
		// and the failure (an ADR that matches nothing) is silent.
		{"internal/pricing", "internal/pricing/store.go", true},
		{"internal/pricing", "internal/pricingx/store.go", false},
		{"docs/adr/README.md", "docs/adr/README.md", true},

		{"", "internal/x.go", false},
		{"internal/**", "", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestOverlap(t *testing.T) {
	files := []string{"internal/pricing/store.go", "cmd/adrlog/main.go", "README.md"}
	got := Overlap([]string{"internal/**", "cmd/**"}, files)
	if len(got) != 2 || got[0] != "internal/pricing/store.go" || got[1] != "cmd/adrlog/main.go" {
		t.Errorf("Overlap = %v", got)
	}
	if n := Overlap([]string{"nothing/**"}, files); n != nil {
		t.Errorf("expected no overlap, got %v", n)
	}
}
