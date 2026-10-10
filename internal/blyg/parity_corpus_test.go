package blyg

import "testing"

// parityCorpus is the shared input set for TestReferenceParity: the edges
// where two normalizers or flatteners are likeliest to disagree.
func parityCorpus(t *testing.T) []parityCase {
	t.Helper()
	var cs []parityCase
	html := func(name, h string) { cs = append(cs, parityCase{Op: "selectionText", Name: name, HTML: h}) }
	// The reference decodes a fixed list of entities, maps curly quotes to
	// ASCII, and leaves numeric and other named entities as written. blygd
	// decodes every entity, as HTML text content does (§10.2). Neither
	// renderer emits such entities (both write the characters), so the two
	// agree on each other's content; a third client's HTML would not.
	entities := func(name, h string) {
		cs = append(cs, parityCase{Op: "selectionText", Name: name, HTML: h,
			Departure: "entity decoding: reference decodes a fixed subset (spec finding: §10.2 text content is underspecified)"})
	}
	quote := func(name, q string) { cs = append(cs, parityCase{Op: "quote", Name: name, Quote: q}) }
	locate := func(name, h, q string) { cs = append(cs, parityCase{Op: "locate", Name: name, HTML: h, Quote: q}) }

	// §10.2 text content of published HTML.
	html("soft wrap", "<p>one\ntwo  three</p>")
	html("two paragraphs", "<p>a</p>\n<p>b</p>")
	html("empty paragraph", "<p>a</p><p></p><p>b</p>")
	html("inline tags", "<p><strong>x</strong>. <em>y</em> <code>z</code></p>")
	html("named entities", "<p>a &amp; b &lt;c&gt; &quot;d&quot; &#39;e&#39;</p>")
	entities("typographic entities", "<p>&ldquo;q&rdquo; &rsquo;s &hellip; &mdash; &ndash; &nbsp;x</p>")
	entities("numeric entities", "<p>&#8217;&#x2019; &#169;</p>")
	entities("uncommon named entity", "<p>&copy; &eacute; &trade;</p>")
	html("double-escaped", "<p>&amp;lt;tag&amp;gt;</p>")
	html("list", "<ul>\n<li>one</li>\n<li><p>two</p></li>\n</ul>")
	html("nested blockquote", "<blockquote><p>outer</p><blockquote><p>inner</p></blockquote></blockquote>")
	html("pre", "<pre><code>line 1\nline 2\n</code></pre>")
	html("br", "<p>x<br>y<br/>z<br />w</p>")
	html("headings and hr", "<h1>Title</h1>\n<hr>\n<h2>Sub</h2>")
	html("table", "<table><tr><td>a</td><td>b</td></tr><tr><td>c</td></tr></table>")
	html("script and style", "<p>a</p><script>bad()</script><style>p{}</style><p>b</p>")
	html("unicode spaces", "<p>a b c d​e</p>")
	html("image only", `<p><img src="x.png" alt="alt text"></p>`)
	html("emoji and astral", "<p>👩‍💻 𝔘 é</p>")
	html("div and section", "<div>a</div><section>b</section><article>c</article>")
	html("figure and dl", "<figure>f<figcaption>cap</figcaption></figure><dl><dt>t</dt><dd>d</dd></dl>")
	html("bake", `<blockquote class="blyg-transclusion" data-blyg-id="x" data-blyg-version="1">`+"\n<p>quoted</p>\n</blockquote>\n<p>after</p>")
	html("attribute with >", `<p><a title="a>b" href="#">link</a> text</p>`)

	// A quote as the author writes it, rendered by each client's own
	// markdown and normalized: the needle of the check.
	quote("plain", "Stigmergy is what a protocol looks like")
	quote("soft wrapped", "Stigmergy is what a protocol\nlooks like from inside")
	quote("two paragraphs", "first para\n\nsecond para")
	quote("emphasis", "a *quiet* **claim**")
	quote("link", "see [the post](https://x.test/) now")
	quote("code span", "run `go test` first")
	quote("ampersand and angle", "cats & dogs < birds")
	quote("hard break", "line one  \nline two")
	quote("backslash escape", `not \*emphasis\*`)
	quote("bare url", "visit https://x.test/a?b=1 today")
	quote("smart punctuation", "it’s “quoted” — really…")
	quote("list", "- one\n- two")
	quote("heading", "# Heading")
	quote("html-looking", "<b>bold</b> stays text")
	quote("trailing spaces", "words   ")

	// The whole check, against HTML each renderer produces.
	target := "<p>Stigmergy is what a <em>protocol</em> looks like\nfrom inside.</p>\n<p>Second &amp; last, “quoted”.</p>\n"
	locate("found across tags", target, "what a protocol looks like from inside")
	locate("found across blocks", target, "from inside.\n\nSecond & last")
	locate("welded blocks fail", target, "from inside. Second")
	locate("entity in target", target, "Second & last, “quoted”")
	locate("not present", target, "absent words")
	locate("prefix and suffix", "<p>"+repeat("x", 40)+" needle "+repeat("y", 40)+"</p>", "needle")
	locate("astral context", "<p>𝔘𝔘𝔘 needle 𝔘𝔘𝔘</p>", "needle")

	// §5.6 rule 6, on bakes in the shared wire format.
	o := "https://ex.test/me/"
	q := "7c9wk2mhq0v3xj8tn5rzfd41bg"
	r := "1vgtgz0gq5b2c9k7d3m8r4n6xy"
	whole := `<blockquote class="blyg-transclusion" data-blyg-id="` + q + `" data-blyg-version="3">` + "\n<p>Quoted <em>words</em>.</p>\n\n</blockquote>"
	remote := `<blockquote class="blyg-transclusion" data-blyg-id="` + r + `" data-blyg-version="1" data-blyg-origin="https://pi.test/">` + "\n<p>Remote words.</p>\n\n</blockquote>"
	partial := `<blockquote class="blyg-transclusion blyg-partial" data-blyg-id="` + q + `" data-blyg-version="3">` + "\n<p>Quoted words.</p>\n</blockquote>"
	flatten := func(name, md, h, dep string) {
		cs = append(cs, parityCase{Op: "flatten", Name: name, MD: md, HTML: h, Origin: o, Departure: dep})
	}
	flatten("whole quote between blank lines", "Mine.\n\n![["+q+"]]\n\nAfter.", "<p>Mine.</p>\n"+whole+"\n<p>After.</p>\n", "")
	flatten("remote quote", "![["+r+"]]", remote+"\n", "")
	flatten("partial quote", "![["+q+"]]\n> Quoted words.\n\nAfter.", partial+"\n<p>After.</p>\n", "")
	flatten("prose right after a partial", "![["+q+"]]\n> Quoted words.\nAfter.", partial+"\n<p>After.</p>\n",
		"blygd adds a blank line so the prose does not join the quote by lazy continuation")
	flatten("own link", "See [["+q+"]] here.", `<p>See <a href="`+o+`f/`+q+`/">“Quoted words.”</a> here.</p>`+"\n", "")
	flatten("nested quote", "![["+q+"]]",
		`<blockquote class="blyg-transclusion" data-blyg-id="`+q+`" data-blyg-version="3">`+"\n<p>Outer.</p>\n"+remote+"\n</blockquote>\n", "")
	flatten("generated inline", "Mine and pasted.", `<p>Mine and <span class="blyg-tk-gen">pasted</span>.</p>`+"\n", "")
	flatten("generated block", "Mine.\n\nA block.\n\nTail.", "<p>Mine.</p>\n"+`<div class="blyg-tk-gen"><p>A block.</p>`+"\n</div>\n<p>Tail.</p>\n", "")
	flatten("mismatched bake falls back", "![["+q+"]]\n\n![["+r+"]]", whole+"\n", "")
	flatten("list and code in a quote", "![["+q+"]]",
		`<blockquote class="blyg-transclusion" data-blyg-id="`+q+`" data-blyg-version="3">`+"\n<ul>\n<li>one</li>\n<li>two</li>\n</ul>\n<pre><code>x := 1\n</code></pre>\n</blockquote>\n", "")
	flatten("markdown specials in quoted text", "![["+q+"]]",
		`<blockquote class="blyg-transclusion" data-blyg-id="`+q+`" data-blyg-version="3">`+"\n<p>a * b _c_ [d] &lt;e&gt; `f`</p>\n</blockquote>\n", "")
	return cs
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
