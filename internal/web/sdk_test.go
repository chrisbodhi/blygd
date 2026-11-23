package web

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Swift SDK's client (sdk/swift) is generated from this document and
// committed, and each generated file names the hash of the document it
// came from. A change to openapi.json regenerates it in the same change.
// This checks the stamp; scripts/swift-sdk.sh check, which needs Swift,
// checks the code.
func TestSwiftSDKIsCurrent(t *testing.T) {
	want := fmt.Sprintf("(sha256 %x)", sha256.Sum256(openAPIDoc))
	files, err := filepath.Glob(filepath.Join("..", "..", "sdk", "swift", "Sources", "Blygd", "Generated", "*.swift"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no generated Swift SDK sources; run make sdk")
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s was generated from another openapi.json; run make sdk", filepath.Base(f))
		}
	}
}
