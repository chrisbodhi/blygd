package blyg

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
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

func TestPagesNeverPassMathThroughUnexamined(t *testing.T) {
	// §14 (decision #105): a math element is kept under an allowlist,
	// redrawn, replaced by its TeX as text, or dropped. Ours replace or drop.
	formula := `<p>Euler: <math><semantics><mrow><msup><mi>e</mi><mrow><mi>i</mi><mi>π</mi></mrow></msup><mo>+</mo><mn>1</mn><mo>=</mo><mn>0</mn></mrow>` +
		`<annotation encoding="application/x-tex">e^{i\pi} + 1 = 0</annotation></semantics></math>, famously.</p>`
	got := string(PageHTML(formula))
	if !strings.Contains(got, `<code>e^{i\pi} + 1 = 0</code>, famously.`) || strings.Contains(got, "msup") || strings.Contains(got, "<math") {
		t.Errorf("inline formula: %s", got)
	}
	block := `<math display="block"><semantics><mi>x</mi><annotation encoding="application/x-tex">x &lt; y</annotation></semantics></math>`
	if got := string(PageHTML(block)); got != "<pre><code>x &lt; y</code></pre>" {
		t.Errorf("display formula: %q", got)
	}
	// No annotation: dropped whole, not left as loose presentation text.
	if got := string(PageHTML(`<p>a<math><mi>x</mi><mn>2</mn></math>b</p>`)); got != "<p>ab</p>" {
		t.Errorf("formula without TeX: %q", got)
	}
	// The classic: markup inert in one vocabulary and live in the other.
	for _, attack := range []string{
		`<math><mtext><table><mglyph><style><img src=x onerror=alert(1)>`,
		`<math><annotation-xml encoding="text/html"><img src=x onerror=alert(1)></annotation-xml></math>`,
		`<math><maction actiontype="statusline" xlink:href="javascript:alert(1)">x</maction></math>`,
		`<math><annotation encoding="application/x-tex"><img src=x onerror=alert(1)></annotation></math>`,
		`<MATH><mi>x</mi></MATH><img src=x onerror=alert(1)>`,
	} {
		got := string(PageHTML(attack))
		for _, bad := range []string{"onerror", "javascript:", "<math", "<style", "annotation"} {
			if strings.Contains(strings.ToLower(got), bad) {
				t.Errorf("%s\n  kept %q: %s", attack, bad, got)
			}
		}
	}
}

func TestHopperFeedIsWellFormedAndPlain(t *testing.T) {
	// §13.5 rules (b) and (d): no blyg: namespace, one guid per membership,
	// <source> for others' members, an excerpt and a link. An imported
	// noncharacter, which a reader accepts (§5.1), must not break the XML.
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	body := RenderHopperFeed(HopperFeed{Title: "Reads", Page: "https://h.test/~a/h/reads/", Origin: "https://h.test/~a/", Site: "A's", BuiltAt: at},
		[]HopperFeedEntry{
			{Identity: "https://b.test/", ID: remoteID, Link: "https://b.test/f/" + remoteID + "/", Excerpt: "Theirs ￾ & <b>", SourceFeed: "https://b.test/feed.xml", Source: "B", Author: "Bee", AddedAt: at},
			{Identity: "https://h.test/~a/", ID: ownID, Link: "https://h.test/~a/f/" + ownID + "/", Excerpt: "Mine", Own: true, AddedAt: at.Add(-time.Hour)},
		})
	d := xml.NewDecoder(bytes.NewReader(body))
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("not well-formed XML: %v\n%s", err, body)
		}
	}
	s := string(body)
	if strings.Contains(s, "blyg:") || strings.Contains(s, Namespace) || strings.Count(s, "<source ") != 1 || strings.Count(s, "<guid ") != 2 {
		t.Errorf("feed:\n%s", s)
	}
	if HopperGUID("p", "o", "i") == HopperGUID("p", "o", "j") || HopperGUID("p", "o", "i") != HopperGUID("p", "o", "i") {
		t.Error("a guid is derived from the hopper, origin and id, and stable")
	}
}

func TestAnUnclosedMathCannotEatTheQuotersWords(t *testing.T) {
	// Review of PR #5: a stranger's unclosed <math> runs at most to the end
	// of the element it was opened in, never into the quoter's own words.
	in := `<blockquote class="blyg-transclusion" data-blyg-id="x"><p>q <math><mi>x</p></blockquote><p>My words</p>`
	got := string(PageHTML(in))
	if !strings.Contains(got, "<p>My words</p>") || !strings.Contains(got, "<p>q </p></blockquote>") {
		t.Errorf("got %s", got)
	}
	in = `<p>Mine.</p><blockquote class="blyg-transclusion" data-blyg-id="x"><math><annotation encoding="application/x-tex">a+b</annotation>` +
		`<mi>a</mi></blockquote><p>A rebuttal.</p>`
	got = string(PageHTML(in))
	if !strings.Contains(got, "<code>a+b</code></blockquote>") || !strings.Contains(got, "<p>A rebuttal.</p>") || !strings.Contains(got, "<p>Mine.</p>") {
		t.Errorf("got %s", got)
	}
}

func TestRawTextTagsInsideMathCannotEatTheQuotersWords(t *testing.T) {
	// Review of PR #5: inside MathML a browser does not switch to raw text
	// on these, so neither may the skip, or it runs to the end of input.
	for _, tag := range []string{"<style>", "<textarea>", "<title>", "<script>", "<plaintext>", "<mtext><xmp>", "<noscript>", "<iframe>", "<noembed>", "<noframes>"} {
		in := `<blockquote class="blyg-transclusion" data-blyg-id="x"><p>q <math>` + tag + `</p></blockquote><p>My words</p>`
		if got := string(PageHTML(in)); !strings.Contains(got, "<p>My words</p>") {
			t.Errorf("%s: %s", tag, got)
		}
	}
}
