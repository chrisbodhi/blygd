package web

import (
	"regexp"
	"testing"

	"github.com/chrisbodhi/blygd/internal/archtest"
)

// The HTTP layer holds no SQL: each table's shape is known to the package
// that owns it, which also decides which plane a query may touch. A
// handler that needs data asks the domain package for it.
func TestHandlersHoldNoSQL(t *testing.T) {
	archtest.Check(t, ".", archtest.Rule{
		Banned: regexp.MustCompile(`\b(FROM|JOIN|INTO|UPDATE)\s+[a-z_]+\b`),
		Why:    "SQL in the HTTP layer: move it into the package that owns the table",
	})
}
