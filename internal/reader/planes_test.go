package reader

import (
	"regexp"
	"testing"

	"github.com/chrisbodhi/blygd/internal/archtest"
)

// The reader reads a same-instance origin through its rendered surface,
// never through its plane-1 tables, which would skip §13.3–13.4 for local
// origins only (PLAN.md, Phase 2 item 8). And it never writes the surface.
func TestReaderNeverTouchesPlaneOne(t *testing.T) {
	archtest.Check(t, ".", archtest.Rule{
		Banned: regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE)\s+(items|item_versions|media|surface_files)\b`),
		Why:    "this queries plane 1 directly: read a local origin through LocalFirst",
	})
}
