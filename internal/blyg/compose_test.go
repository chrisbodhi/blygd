package blyg

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	ownID    = "7c9wk2mhq0v3xj8tn5rzfd41bg"
	remoteID = "1vgtgz0gq5b2c9k7d3m8r4n6xy"
	headedID = "0000000000000000000000000h"
	draftID  = "00000000000000000000000000"
	page     = "https://ex.test/me/t/zzzzzzzzzzzzzzzzzzzzzzzzzz/"
)

func fakeResolve(id string, use Use) (*Target, error) {
	switch id {
	case ownID:
		return &Target{ID: ownID, Kind: "fragment", Version: 3,
			ContentHTML: "<p>Stigmergy is what a protocol looks like\nfrom inside.</p>\n<p>Second &amp; last.</p>\n",
			Page:        "https://ex.test/me/f/" + ownID + "/"}, nil
	case remoteID:
		if use == UseSource {
			return nil, &Unresolved{"not an own item"}
		}
		return &Target{ID: remoteID, Kind: "fragment", Version: 1, Origin: "https://pi.test/",
			ContentHTML: `<p>See <a href="notes/">notes</a> and <img src="/m.png" alt=""></p>`,
			Page:        "https://pi.test/f/" + remoteID + "/"}, nil
	case headedID:
		return &Target{ID: headedID, Kind: "thread", Version: 2,
			ContentHTML: "<h1>On <em>stigmergy</em></h1>\n<p>Body.</p>\n",
			Page:        "https://ex.test/me/t/" + headedID + "/"}, nil
	}
	return nil, &Unresolved{"unknown item"}
}

func compose(t *testing.T, md string, thread bool, gen ...GenMeta) *Composed {
	t.Helper()
	out, err := Compose(ComposeInput{ContentMD: md, Thread: thread, PageURL: page, Resolve: fakeResolve, Generation: gen})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func clean(t *testing.T, out *Composed) {
	t.Helper()
	if len(out.Problems) > 0 {
		t.Fatalf("unexpected problems: %+v", out.Problems)
	}
	if strings.ContainsAny(out.ContentHTML, "") {
		t.Fatalf("a sentinel survived: %q", out.ContentHTML)
	}
}

func TestWholeTransclusionBake(t *testing.T) {
	out := compose(t, "Before.\n\n![["+ownID+"]]\n\nAfter.", true)
	clean(t, out)
	want := "<p>Before.</p>\n\n" +
		`<blockquote class="blyg-transclusion" data-blyg-id="` + ownID + `" data-blyg-version="3">` + "\n" +
		"<p>Stigmergy is what a protocol looks like\nfrom inside.</p>\n<p>Second &amp; last.</p>\n\n</blockquote>\n" +
		"<p>After.</p>\n"
	if out.ContentHTML != want {
		t.Fatalf("bake:\n%s\nwant:\n%s", out.ContentHTML, want)
	}
	b, _ := json.Marshal(out.Transclusions)
	if string(b) != `[{"id":"`+ownID+`","version":3}]` {
		t.Fatalf("own-origin entry must omit origin (§10.3): %s", b)
	}
}

func TestRemoteBakeCarriesOriginAndIsSelfContained(t *testing.T) {
	out := compose(t, "![["+remoteID+"]]", true)
	clean(t, out)
	if !strings.Contains(out.ContentHTML, `data-blyg-version="1" data-blyg-origin="https://pi.test/">`) {
		t.Fatalf("remote bake needs data-blyg-origin: %s", out.ContentHTML)
	}
	for _, abs := range []string{`href="https://pi.test/notes/"`, `src="https://pi.test/m.png"`} {
		if !strings.Contains(out.ContentHTML, abs) {
			t.Errorf("relative URL not resolved against the source origin, want %s in %s", abs, out.ContentHTML)
		}
	}
	if out.Transclusions[0].Origin != "https://pi.test/" {
		t.Fatalf("remote entry needs origin: %+v", out.Transclusions[0])
	}
}

func TestPartialTransclusion(t *testing.T) {
	// The selection spans a soft wrap in the source and a paragraph break.
	md := "![[" + ownID + "]]\n> looks like from inside.\n>\n> Second & last.\n\nCommentary."
	out := compose(t, md, true)
	clean(t, out)
	want := `<blockquote class="blyg-transclusion blyg-partial" data-blyg-id="` + ownID + `" data-blyg-version="3">` + "\n" +
		"<p>looks like from inside.</p>\n<p>Second &amp; last.</p>\n</blockquote>\n<p>Commentary.</p>\n"
	if out.ContentHTML != want {
		t.Fatalf("partial bake:\n%s\nwant:\n%s", out.ContentHTML, want)
	}
	sel := out.Transclusions[0].Selector
	if sel.Exact != "looks like from inside.\nSecond & last." || sel.Prefix != "Stigmergy is what a protocol " || sel.Suffix != "" {
		t.Fatalf("selector: %+v", sel)
	}
}

func TestPartialMustBeInTheTarget(t *testing.T) {
	out := compose(t, "![["+ownID+"]]\n> Stigmergy is what a protocol looks like from outside.", true)
	if len(out.Problems) != 1 || !strings.Contains(out.Problems[0].Reason, "not in version 3") {
		t.Fatalf("want one substring failure, got %+v", out.Problems)
	}
	// A quote that welds two paragraphs together is not in the text either.
	out = compose(t, "![["+ownID+"]]\n> from inside. Second", true)
	if len(out.Problems) != 1 {
		t.Fatalf("a quote across a block boundary must keep the break: %+v", out.Problems)
	}
}

func TestBlankLineDetachesTheQuote(t *testing.T) {
	out := compose(t, "![["+ownID+"]]\n\n> My own quotation, not in the target.", true)
	clean(t, out)
	if strings.Contains(out.ContentHTML, "blyg-partial") || !strings.Contains(out.ContentHTML, "<blockquote>\n<p>My own quotation") {
		t.Fatalf("detached quote must stay the author's: %s", out.ContentHTML)
	}
}

func TestLazyContinuationBelongsToTheQuote(t *testing.T) {
	// §10.1 (decision #76): the blockquote's extent is CommonMark's, so the
	// second line is part of the selection although it has no marker.
	out := compose(t, "![["+ownID+"]]\n> looks like\nfrom inside.\n\nCommentary.", true)
	clean(t, out)
	if sel := out.Transclusions[0].Selector; sel == nil || !strings.Contains(sel.Exact, "from inside.") {
		t.Fatalf("lazy line left out of the selection: %+v", sel)
	}
	if strings.Contains(out.ContentHTML, "<p>from inside.</p>") {
		t.Fatalf("lazy line fell through as prose: %s", out.ContentHTML)
	}
	// A line that starts another block is not a continuation.
	out = compose(t, "![["+ownID+"]]\n> looks like\n---", true)
	clean(t, out)
	if !strings.Contains(out.ContentHTML, "<hr") || out.Transclusions[0].Selector.Exact != "looks like" {
		t.Fatalf("a thematic break must end the quote: %s", out.ContentHTML)
	}
}

func TestCRLFLinesEndLikeLF(t *testing.T) {
	// §10.1 (decision #76): a line ends at LF or CRLF alike.
	out := compose(t, "Before.\r\n\r\n![["+ownID+"]]\r\n\r\nAfter.", true)
	clean(t, out)
	if len(out.Transclusions) != 1 || out.Transclusions[0].Selector != nil {
		t.Fatalf("a CRLF directive line must bake whole: %+v %s", out.Transclusions, out.ContentHTML)
	}
	out = compose(t, "![["+ownID+"]]\r\n> looks like from inside.\r\n\r\nCommentary.", true)
	clean(t, out)
	if len(out.Transclusions) != 1 || out.Transclusions[0].Selector == nil || out.Transclusions[0].Selector.Exact != "looks like from inside." {
		t.Fatalf("a CRLF partial quote: %+v %s", out.Transclusions, out.ContentHTML)
	}
	if !HasThreadGrammar("![[" + ownID + "]]\r\n") {
		t.Fatal("HasThreadGrammar must see a CRLF directive line")
	}
	if c := compose(t, "![["+ownID+"@v2]]\r\n", true); len(c.Problems) != 1 {
		t.Fatalf("a CRLF reserved line must be refused: %+v", c.Problems)
	}
}

func TestGrammarIsInertInCode(t *testing.T) {
	md := "```\n![[" + ownID + "]]\n[[" + draftID + "]]\n```\n\n" +
		"- ```md\n  ![[" + draftID + "]]\n  ```\n\n" +
		"Inline `[[" + draftID + "]]` and ![[" + draftID + "]] mid-line."
	out := compose(t, md, true)
	clean(t, out)
	if len(out.Transclusions) != 0 {
		t.Fatalf("nothing in code may resolve: %+v", out.Transclusions)
	}
	if !strings.Contains(out.ContentHTML, "<code>[["+draftID+"]]</code>") || !strings.Contains(out.ContentHTML, "![["+draftID+"]] mid-line") {
		t.Fatalf("inert grammar must render as text: %s", out.ContentHTML)
	}
}

func TestReservedAndUnknown(t *testing.T) {
	out := compose(t, "![["+ownID+"@v2]]\n\n![["+draftID+"]]\n> quoted", true)
	if len(out.Problems) != 2 || !strings.Contains(out.Problems[0].Reason, "reserved") || out.Problems[1].Reason != "unknown item" {
		t.Fatalf("problems: %+v", out.Problems)
	}
	if strings.Contains(out.ContentHTML, "quoted") {
		t.Fatalf("a failed directive's attached quote must not fall through as prose: %s", out.ContentHTML)
	}
}

func TestFragmentsDoNotTransclude(t *testing.T) {
	out := compose(t, "![["+ownID+"]]", false)
	if len(out.Problems) != 1 || !strings.Contains(out.Problems[0].Reason, "only threads transclude") {
		t.Fatalf("problems: %+v", out.Problems)
	}
}

func TestPlainLinks(t *testing.T) {
	md := "See [[" + headedID + "]] and [[" + ownID + "]], [x [[" + ownID + "]]](https://o.test/), `[[" + ownID + "]]`."
	out := compose(t, md, false)
	clean(t, out)
	h := out.ContentHTML
	for _, want := range []string{
		`<a href="https://ex.test/me/t/` + headedID + `/">On stigmergy</a>`,
		`<a href="https://ex.test/me/f/` + ownID + `/">“Stigmergy is what a protocol looks like from inside. Second…”</a>`,
		`<a href="https://o.test/">x “Stigmergy`, // inside another anchor: label as text, no nesting
		`<code>[[` + ownID + `]]</code>`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %s in\n%s", want, h)
		}
	}
	if strings.Count(h, "<a ") != 3 {
		t.Errorf("want exactly three anchors: %s", h)
	}
	if len(out.Transclusions) != 0 {
		t.Fatal("a link is silent on the wire (§10.1)")
	}
	if out.ContentMD != md {
		t.Fatal("content_md keeps the links as written")
	}
}

func TestUnresolvableLinkIsAPublishError(t *testing.T) {
	out := compose(t, "See [["+draftID+"]].", false)
	if len(out.Problems) != 1 || out.Problems[0].At != "[["+draftID+"]]" {
		t.Fatalf("problems: %+v", out.Problems)
	}
}

func TestGeneratedSpans(t *testing.T) {
	md := "Intro [TK]impyrt claude-x=pasted words[/TK] end.\n\n" +
		"[TK]summarize ![[" + ownID + "]][=]A **block** with [[" + headedID + "]].\n\nTwo paras.[/TK]\n\n" +
		"Tail."
	out := compose(t, md, false, GenMeta{}, GenMeta{Model: "m-1", At: "2026-10-08T00:00:00Z"})
	clean(t, out)
	wantMD := "Intro pasted words end.\n\nA **block** with [[" + headedID + "]].\n\nTwo paras.\n\nTail."
	if out.ContentMD != wantMD {
		t.Fatalf("stripped content_md:\n%q\nwant\n%q", out.ContentMD, wantMD)
	}
	h := out.ContentHTML
	if !strings.Contains(h, `<p>Intro <span class="blyg-tk-gen">pasted words</span> end.</p>`) {
		t.Errorf("inline span: %s", h)
	}
	if !strings.Contains(h, `<div class="blyg-tk-gen"><p>A <strong>block</strong> with <a href="https://ex.test/me/t/`+headedID+`/">On stigmergy</a>.</p>`+"\n<p>Two paras.</p>\n</div>") {
		t.Errorf("block div, with its link resolved: %s", h)
	}
	g, _ := json.Marshal(out.Generated)
	want := `[{"sources":[],"model":"claude-x"},{"sources":[{"id":"` + ownID + `","version":3}],"model":"m-1","at":"2026-10-08T00:00:00Z"}]`
	if string(g) != want {
		t.Fatalf("generated:\n%s\nwant\n%s", g, want)
	}
}

// WireMD is what Compose would put on the wire, without resolving
// anything; the studio compares it with a published version.
func TestWireMDMatchesCompose(t *testing.T) {
	for _, md := range []string{
		"Plain *words*.",
		"Intro [TK]impyrt claude-x=pasted words[/TK] end.\n\n[TK]summarize ![[" + ownID + "]][=]A **block**.[/TK]\n\nTail.",
		"a [TK]impyrt=b\n\nc[/TK] d",
		"`[TK]x[/TK]`",
	} {
		got, ok := WireMD(md)
		if out := compose(t, md, false, GenMeta{}, GenMeta{Model: "m-1"}); !ok || got != out.ContentMD {
			t.Errorf("%q: WireMD %q (ok %v), Compose %q", md, got, ok, out.ContentMD)
		}
	}
	for _, md := range []string{"x [TK]never closed", "[TK]write something[/TK]"} {
		if _, ok := WireMD(md); ok {
			t.Errorf("%q: WireMD ok for a draft that cannot publish", md)
		}
	}
}

func TestGeneratedSpanAcrossParagraphsStaysNested(t *testing.T) {
	out := compose(t, "a [TK]impyrt=b\n\nc[/TK] d", false)
	clean(t, out)
	want := `<p>a <span class="blyg-tk-gen">b</span></p>` + "\n" + `<p><span class="blyg-tk-gen">c</span> d</p>` + "\n"
	if out.ContentHTML != want {
		t.Fatalf("got %q\nwant %q", out.ContentHTML, want)
	}
}

func TestTKProblems(t *testing.T) {
	cases := map[string]string{
		"x [TK]never closed":                         "never closed",
		"[TK]a [TK]b[/TK]":                           "cannot nest",
		"[TK]write something[/TK]":                   "no output",
		"[TK]impyrt=a\n![[" + ownID + "]]\nb[/TK]":   "inside generated text",
		"[TK]from ![[" + remoteID + "]][=]text[/TK]": "not an own item",
	}
	for md, want := range cases {
		out := compose(t, md, true)
		if len(out.Problems) == 0 || !strings.Contains(out.Problems[0].Reason, want) {
			t.Errorf("%q: want %q, got %+v", md, want, out.Problems)
		}
	}
	// [TK] inside code is an example, not a scope.
	out := compose(t, "`[TK]x[/TK]`", false)
	clean(t, out)
	if len(out.Generated) != 0 {
		t.Fatal("a [TK] in code is not a scope")
	}
}

func TestSelectionText(t *testing.T) {
	cases := map[string]string{
		"<p>a\n  b</p><p></p><p>c &amp; d</p>":       "a b\nc & d",
		"<ul><li>one</li><li><em>two</em></li></ul>": "one\ntwo",
		"<p>x<br>y</p><script>z</script>":            "x\ny",
	}
	for in, want := range cases {
		if got := SelectionText(in); got != want {
			t.Errorf("SelectionText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasThreadGrammar(t *testing.T) {
	for md, want := range map[string]bool{
		"plain":                     false,
		"`[[" + ownID + "]]`":       false,
		"x [[" + ownID + "]] y":     true,
		"![[" + ownID + "]]":        true,
		"inline ![[" + ownID + "]]": false,
		"![[" + ownID + "@v1]]":     true,
	} {
		if got := HasThreadGrammar(md); got != want {
			t.Errorf("HasThreadGrammar(%q) = %v", md, got)
		}
	}
}

func TestLabelledDirective(t *testing.T) {
	for line, want := range map[string]string{
		"![[" + ownID + "]]":                  "|",
		"  ![[" + ownID + "]] \r":             "|",
		"![[" + ownID + " | the opening ]]":   "|the opening",
		"![[" + ownID + "|a | b]]":            "|a | b",
		"![[" + ownID + " | ]]":               "|",
		"![[" + ownID + " | a]] b":            "",
		"![[" + ownID + " | a]]b]]":           "",
		"![[" + ownID + " x]]":                "",
		"![[0000000000000000000000000u | x]]": "",
		"text ![[" + ownID + "]]":             "",
	} {
		id, label, ok := labelledDirective(line)
		got := ""
		if ok {
			got = "|" + label
			if id != ownID {
				t.Errorf("%q: id %q", line, id)
			}
		}
		if got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}

func TestHasOwnWords(t *testing.T) {
	whole := BakeWhole(&Target{ID: ownID, Version: 3}, "<p>w</p>")
	part := func(sel string) string { return BakePartial(&Target{ID: ownID, Version: 3}, sel) }
	for _, c := range []struct {
		name, md, html string
		want           bool
	}{
		{"a bare directive", "![[" + ownID + "]]\n", whole, false},
		{"twenty directives, labelled", strings.Repeat("![["+ownID+" | x]]\n\n", 20), strings.Repeat(whole, 20), false},
		{"a partial selection", "![[" + ownID + "]]\n> sel\n", part("sel"), false},
		{"a lazy line the bake selected", "![[" + ownID + "]]\n> sel\nmore\n", part("sel more"), false},
		{"a lazy line the bake did not select", "![[" + ownID + "]]\n> sel\nmine\n", part("sel") + "<p>mine</p>", true},
		{"a quotation after a whole bake", "![[" + ownID + "]]\n> mine\n", whole + "<blockquote><p>mine</p></blockquote>", true},
		{"a paragraph", "![[" + ownID + "]]\n\nI disagree.", whole + "<p>I disagree.</p>", true},
		{"a directive in code", "```\n![[" + ownID + "]]\n```", "<pre><code>![[" + ownID + "]]</code></pre>", true},
		{"bakes that do not line up", "![[" + ownID + "]]\n> sel\nmore", "", false},
	} {
		if got := HasOwnWords(c.md, c.html); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPageHTML(t *testing.T) {
	bake := `<blockquote class="blyg-transclusion blyg-partial" data-blyg-id="` + remoteID + `" data-blyg-version="1" data-blyg-origin="https://pi.test/">` +
		`<p onclick="x()">hi <script>alert(1)</script><a href="javascript:alert(1)">j</a> <span class="blyg-tk-gen evil">g</span></p><iframe src="https://pi.test/"></iframe></blockquote>`
	got := string(PageHTML(bake))
	for _, bad := range []string{"script", "onclick", "javascript:", "iframe", "evil"} {
		if strings.Contains(got, bad) {
			t.Errorf("page HTML kept %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, `<blockquote class="blyg-transclusion blyg-partial" data-blyg-id="`+remoteID+`" data-blyg-version="1" data-blyg-origin="https://pi.test/">`) {
		t.Errorf("the bake's wire attributes must survive: %s", got)
	}
	// Our own renderer's output passes through unchanged.
	own := compose(t, "Hi *there* [x](https://o.test/a?b=1&c=2) `code`\n\n- one\n\n> q", false).ContentHTML
	if string(PageHTML(own)) != own {
		t.Errorf("own HTML changed:\n%s\n%s", own, PageHTML(own))
	}
}
