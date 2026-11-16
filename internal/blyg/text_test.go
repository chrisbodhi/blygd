package blyg

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextFault(t *testing.T) {
	// §5.1 (decision #103): C0 controls other than tab, LF and CR are not
	// text; U+FFFE and U+FFFF are a publisher's SHOULD NOT.
	for _, ok := range []string{"", "plain", "tab\there\r\nand\nlines", "é and é", "\u007f", "�"} {
		if f := PublishFault(ok); f != "" {
			t.Errorf("%q: %s", ok, f)
		}
	}
	for _, bad := range []string{"nul\x00", "bell\a", "esc\x1b[31m", "\x0bvt", "bad \xff byte"} {
		if TextFault(bad) == "" || PublishFault(bad) == "" {
			t.Errorf("%q passed", bad)
		}
	}
	for _, nonchar := range []string{"x￾", "x￿"} {
		if TextFault(nonchar) != "" || PublishFault(nonchar) == "" {
			t.Errorf("%q: a reader accepts it, a publisher refuses it", nonchar)
		}
	}
}

func TestLoneSurrogate(t *testing.T) {
	cases := map[string]bool{
		`"plain"`:           false,
		`"😀 smile"`:         false,
		`"😀"`:               false,
		`"\\ud800 escaped"`: false,
		`"é"`:               false,
		`"\ud800"`:          true,
		`"\ud800x"`:         true,
		`"\ude00"`:          true,
		`"\ud800𐀀"`:         true,
		`"a\ud83d\n"`:       true,
	}
	for raw, want := range cases {
		if got := LoneSurrogate([]byte(raw)); got != want {
			t.Errorf("%s: got %v, want %v", raw, got, want)
		}
	}
}

func TestTruncateCountsCodePointsAndKeepsClusters(t *testing.T) {
	// §5.3 (decision #103): a cap in characters counts code points; a cut
	// never splits an extended grapheme cluster.
	family := "👨‍👩‍👧" // one cluster, five code points
	s := "ab" + family + "cd"
	if got, ok := Truncate(s, 2); got != "ab" || !ok {
		t.Errorf("before the cluster: %q", got)
	}
	for n := 3; n < 7; n++ {
		if got, _ := Truncate(s, n); got != "ab" {
			t.Errorf("n=%d split the cluster: %q", n, got)
		}
	}
	if got, _ := Truncate(s, 7); got != "ab"+family {
		t.Errorf("n=7: %q", got)
	}
	if got, ok := Truncate(s, 9); got != s || ok {
		t.Errorf("no cut needed: %q %v", got, ok)
	}
	combining := strings.Repeat("é", 5) // five clusters, ten code points
	if got := Clip(combining, 4); got != "é"+"…" || utf8.RuneCountInString(got) > 4 {
		t.Errorf("Clip: %q", got)
	}
	if got := Clip("abcd", 4); got != "abcd" {
		t.Errorf("Clip at the cap: %q", got)
	}
}

func TestUnpairedBidi(t *testing.T) {
	for s, want := range map[string]bool{
		"plain":          false,
		"⁧عربى⁩ ok":      false,
		"‮evil":          true,
		"close‬":         true,
		"⁦open\nclosed⁩": true,
	} {
		if UnpairedBidi(s) != want {
			t.Errorf("%q: want %v", s, want)
		}
	}
}

func TestComposeRefusesWhatIsNotText(t *testing.T) {
	out := compose(t, "a bell\a here", false)
	if len(out.Problems) != 1 || !strings.Contains(out.Problems[0].Reason, "U+0007") {
		t.Fatalf("a C0 control must be a publish error: %+v", out.Problems)
	}
	out = compose(t, "café ‮evil", false)
	clean(t, out)
	if len(out.Warnings) != 2 {
		t.Fatalf("want NFC and bidi warnings: %+v", out.Warnings)
	}
	out = compose(t, "café", false)
	if len(out.Warnings) != 0 {
		t.Fatalf("NFC text warned: %+v", out.Warnings)
	}
}
