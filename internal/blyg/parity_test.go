package blyg

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Differential tests against the reference client (blygger-studio): the
// same inputs through its TypeScript and our Go, outputs compared. They
// cover what another client must agree on byte for byte: §10.2's text
// content (any reader may re-run the partial check) and §5.6 rule 6's
// fork flattening. Run with BLYGD_REFERENCE_DIR set to a built checkout
// (npm ci && npm run build); skipped otherwise.

type parityCase struct {
	Op        string      `json:"op"`
	Name      string      `json:"-"`
	HTML      string      `json:"html,omitempty"`
	Quote     string      `json:"quote,omitempty"`
	MD        string      `json:"md,omitempty"`
	Origin    string      `json:"origin,omitempty"`
	Generated []Generated `json:"generated,omitempty"`
	// Departure names a deliberate, documented difference (DESIGN.md): the
	// case still runs, and is reported, but does not fail.
	Departure string `json:"-"`
}

func runReference(t *testing.T, cases []parityCase) []json.RawMessage {
	t.Helper()
	dir := os.Getenv("BLYGD_REFERENCE_DIR")
	if dir == "" {
		t.Skip("BLYGD_REFERENCE_DIR not set: no reference client to compare against")
	}
	script, _ := filepath.Abs("testdata/reference_parity.mts")
	in, _ := json.Marshal(cases)
	cmd := exec.Command("node", "--import", "tsx", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BLYGD_REFERENCE_DIR="+dir)
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("reference run: %v\n%s", err, stderr.String())
	}
	var res []json.RawMessage
	if err := json.Unmarshal(out, &res); err != nil || len(res) != len(cases) {
		t.Fatalf("reference output: %v (%d results for %d cases)", err, len(res), len(cases))
	}
	return res
}

// ours computes the Go side of a case, as JSON shaped like the reference's.
func ours(t *testing.T, c parityCase) json.RawMessage {
	t.Helper()
	var v any
	switch c.Op {
	case "selectionText":
		v = SelectionText(c.HTML)
	case "quote":
		h, err := RenderMarkdown(c.Quote, "")
		if err != nil {
			t.Fatal(err)
		}
		v = SelectionText(h)
	case "locate":
		h, _ := RenderMarkdown(c.Quote, "")
		if sel := LocateSelection(c.HTML, SelectionText(h)); sel != nil {
			v = sel
		}
	case "flatten":
		md, exact := FlattenFork(ForkSource{Origin: c.Origin, ContentMD: c.MD, ContentHTML: c.HTML, Generated: c.Generated},
			func(o, id string) string { return o + "items/" + id + ".json" })
		v = map[string]any{"md": md, "exact": exact}
	}
	b, _ := json.Marshal(v)
	return b
}

func TestReferenceParity(t *testing.T) {
	cases := parityCorpus(t)
	ref := runReference(t, cases)
	mismatches := 0
	for i, c := range cases {
		got := ours(t, c)
		if jsonEqual(got, ref[i]) {
			continue
		}
		if c.Departure != "" {
			t.Logf("expected departure, %s %q (%s):\n  ours: %s\n  ref:  %s", c.Op, c.Name, c.Departure, got, ref[i])
			continue
		}
		mismatches++
		t.Errorf("%s %q differs:\n  ours: %s\n  ref:  %s", c.Op, c.Name, got, ref[i])
	}
	t.Logf("%d cases, %d unexpected mismatches", len(cases), mismatches)
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}
