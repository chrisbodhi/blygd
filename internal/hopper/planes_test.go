package hopper

import (
	"regexp"
	"testing"

	"github.com/chrisbodhi/blygd/internal/archtest"
)

// Public hoppers read the mirror, so this package must never write the
// protocol surface (CLAUDE.md, plane separation; §13.5: no protocol
// surface for curation).
func TestHopperNeverWritesTheSurface(t *testing.T) {
	archtest.Check(t, ".", archtest.Rule{
		Banned: regexp.MustCompile(`(?i)\b(INTO|UPDATE|DELETE\s+FROM)\s+(surface_files|items|item_versions|version_media|media)\b`),
		Why:    "this writes plane 1: hoppers only read it",
	})
}
