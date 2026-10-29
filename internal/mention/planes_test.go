package mention

import (
	"regexp"
	"testing"

	"github.com/chrisbodhi/blygd/internal/archtest"
)

// The mention workers stand between the planes. They read plane 1 to know
// a target, and they record verifications inside the studio's render
// transaction (Workers.Display), so they must never write plane 1: only
// the studio renders. They read the mirror in one place, importedPage, for
// a sender's target (§15.2), and only there; nothing else here may, or a
// query inside Display would carry the mirror into a render.
func TestMentionKeepsThePlanesApart(t *testing.T) {
	archtest.Check(t, ".",
		archtest.Rule{
			Banned: regexp.MustCompile(`(?i)\b(INTO|UPDATE|DELETE\s+FROM)\s+(surface_files|items|item_versions|version_media|media|origins|blogroll_entries)\b`),
			Why:    "this writes plane 1: only the studio does, inside its render transaction",
		},
		archtest.Rule{
			Banned:  regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE)\s+(remote_origins|imported_items|imported_pins|subscriptions|reads|hopper_members)\b`),
			Allowed: []string{"importedPage"},
			Why:     "this queries the import mirror: only importedPage may, for a mention's target",
		})
}
