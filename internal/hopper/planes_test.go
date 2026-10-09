package hopper

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Public hoppers read the mirror, so this package must never write the
// protocol surface (CLAUDE.md, plane separation; §13.5: no protocol
// surface for curation).
func TestHopperNeverWritesTheSurface(t *testing.T) {
	banned := regexp.MustCompile(`(?i)\b(INTO|UPDATE|DELETE\s+FROM)\s+(surface_files|items|item_versions|version_media|media)\b`)
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
			t.Errorf("%s writes plane 1 (%q): hoppers only read it", f.Name(), m)
		}
	}
}
