package studio

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The studio writes surface_files, so it must never read the import mirror
// or the overlay (CLAUDE.md, plane separation). This is how imported items
// can never be re-emitted (§13.5): there is no query that could fetch one.
func TestStudioNeverQueriesTheMirror(t *testing.T) {
	banned := regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE)\s+(remote_origins|imported_items|imported_pins|subscriptions|reads|hopper_members)\b`)
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if m := banned.Find(src); m != nil {
			t.Errorf("%s queries the import mirror or overlay (%q): the studio renders plane 1 only", f.Name(), m)
		}
	}
}
