package studio

import (
	"regexp"
	"testing"

	"github.com/chrisbodhi/blygd/internal/archtest"
)

// The studio writes surface_files, so it must never read the import mirror
// or the overlay (CLAUDE.md, plane separation). This is how imported items
// can never be re-emitted (§13.5): there is no query that could fetch one.
// What it may import, so that no other package queries them on its behalf,
// is internal/archtest's import-graph test.
func TestStudioNeverQueriesTheMirror(t *testing.T) {
	archtest.Check(t, ".", archtest.Rule{
		Banned: regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE)\s+(remote_origins|imported_items|imported_pins|subscriptions|reads|hopper_members)\b`),
		Why:    "this queries the import mirror or overlay: the studio renders plane 1 only",
	})
}

func TestNormalizeBlock(t *testing.T) {
	for _, c := range []struct{ kind, in, want string }{
		{"host", "https://Sub.Example.co.uk/x", "sub.example.co.uk"},
		{"domain", "https://a.b.example.co.uk/x", "example.co.uk"},
		{"domain", "evil.example", "evil.example"},
		{"domain", "co.uk", ""},
		{"host", "", ""},
		{"user", "x.example", ""},
	} {
		got, err := normalizeBlock(c.kind, c.in)
		if (c.want == "") != (err != nil) || got != c.want {
			t.Errorf("normalizeBlock(%q, %q) = %q, %v; want %q", c.kind, c.in, got, err, c.want)
		}
	}
}
