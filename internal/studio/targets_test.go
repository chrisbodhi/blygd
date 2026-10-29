package studio

import (
	"testing"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// TargetItem recognizes targets by the paths the studio writes them at
// (§15.3 step 1), so its patterns must accept exactly blyg's.
func TestTargetPatternsMatchThePublishedLayout(t *testing.T) {
	id := blyg.NewID()
	if m := itemDocPath.FindStringSubmatch(blyg.ItemPath(id)); m == nil || m[1] != id {
		t.Errorf("item document %s: %v", blyg.ItemPath(id), m)
	}
	if m := pinDocPath.FindStringSubmatch(blyg.PinPath(id, 12)); m == nil || m[1] != id || m[2] != "12" {
		t.Errorf("pin %s: %v", blyg.PinPath(id, 12), m)
	}
	if itemDocPath.MatchString(blyg.PinPath(id, 1)) || pinDocPath.MatchString(blyg.ItemPath(id)) {
		t.Error("an item document and a pin are told apart")
	}
}
