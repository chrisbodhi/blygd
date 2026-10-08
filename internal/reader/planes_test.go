package reader

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The reader reads a same-instance origin through its rendered surface,
// never through its plane-1 tables, which would skip §13.3–13.4 for local
// origins only (PLAN.md, Phase 2 item 8). And it never writes the surface.
func TestReaderNeverTouchesPlaneOne(t *testing.T) {
	banned := regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE)\s+(items|item_versions|media|surface_files)\b`)
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
			t.Errorf("%s queries plane 1 directly (%q): read a local origin through LocalFirst", f.Name(), m)
		}
	}
}
