package account

import (
	"regexp"
	"testing"

	"github.com/chrisbodhi/blygd/internal/archtest"
)

// Accounts know users and tokens. A new user's origin is the studio's to
// make (OriginMaker), in the same transaction, so it is rendered before
// anyone can see it.
func TestAccountsLeaveTheStudioItsTables(t *testing.T) {
	archtest.Check(t, ".", archtest.Rule{
		Banned: regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE)\s+(surface_files|items|item_versions|version_media|media|origins|blogroll_entries)\b`),
		Why:    "this touches plane 1: ask the studio",
	})
}
