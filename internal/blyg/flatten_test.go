package blyg

import (
	"strings"
	"testing"
)

const srcOrigin = "https://ex.test/me/"

func quoteLink(origin, id string) string { return origin + "items/" + id + ".json" }

func pinned(t *testing.T, md string, gen ...GenMeta) ForkSource {
	t.Helper()
	c := compose(t, md, true, gen...)
	clean(t, c)
	return ForkSource{Origin: srcOrigin, ContentMD: c.ContentMD, ContentHTML: c.ContentHTML, Generated: c.Generated}
}

func TestFlattenForkCopiesTheDocument(t *testing.T) {
	src := pinned(t, "My *own* prose, see [["+headedID+"]].\n\n"+
		"![["+remoteID+"]]\nRight after the whole bake.\n\n"+
		"![["+ownID+"]]\n> from inside.\n"+
		"# After the partial\n\n"+
		"![["+ownID+"]]\n\n> An author quote after a whole bake.")
	md, exact := FlattenFork(src, quoteLink)
	if !exact {
		t.Fatalf("expected an exact copy:\n%s", md)
	}
	want := "My *own* prose, see [On stigmergy](https://ex.test/me/t/" + headedID + "/).\n\n" +
		"> See [notes](https://pi.test/notes/) and ![](https://pi.test/m.png)\n>\n" +
		"> — quoted from [pi.test · v1](https://pi.test/items/" + remoteID + ".json)\n\n" +
		"Right after the whole bake.\n\n" +
		"> from inside.\n>\n> — quoted from [ex.test · v3](https://ex.test/me/items/" + ownID + ".json)\n\n" +
		"# After the partial\n\n" +
		"> Stigmergy is what a protocol looks like from inside.\n>\n> Second & last.\n>\n" +
		"> — quoted from [ex.test · v3](https://ex.test/me/items/" + ownID + ".json)\n\n" +
		"> An author quote after a whole bake."
	if md != want {
		t.Fatalf("fork:\n%s\n\nwant:\n%s", md, want)
	}
	if HasThreadGrammar(md) {
		t.Fatal("a fork must not re-resolve anything (§5.6 rule 6)")
	}
	// The fork composes as plain prose: no bake, no transclusions entry.
	c := compose(t, md, true)
	clean(t, c)
	if strings.Contains(c.ContentHTML, "blyg-transclusion") || len(c.Transclusions) != 0 {
		t.Fatalf("blyg-transclusion survived into the fork: %s", c.ContentHTML)
	}
	// The partial's lines are consumed only because the bake says partial;
	// the author's own quote after a whole bake is kept, apart; and the
	// prose after a quote stays out of it, where it would otherwise join by
	// lazy continuation.
	if strings.Count(md, "An author quote") != 1 {
		t.Fatal("author's quotation lost or duplicated")
	}
	if strings.Contains(c.ContentHTML, "Right after the whole bake.</p>\n</blockquote>") || !strings.Contains(c.ContentHTML, "<p>Right after the whole bake.</p>") {
		t.Fatalf("prose after a flattened quote joined it: %s", c.ContentHTML)
	}
	if !strings.Contains(c.ContentHTML, "<h1>After the partial</h1>") {
		t.Fatalf("the block after a partial quote was lost: %s", c.ContentHTML)
	}
}

func TestFlattenForkReadsTheQuoteExtentFromTheBake(t *testing.T) {
	// Composed under §10.1's CommonMark extent, the lazy line was selected
	// and baked: it is the quote's, and the fork does not repeat it.
	src := pinned(t, "![["+ownID+"]]\n> looks like\nfrom inside.\n\nAfter.")
	md, exact := FlattenFork(src, quoteLink)
	if !exact || strings.Count(md, "from inside.") != 1 || !strings.Contains(md, "> looks like from inside.") {
		t.Fatalf("fork of a lazy quote: %q exact=%v", md, exact)
	}
	// Composed by a client that ended the quote at the first line without
	// `>`, the same line was the author's prose: the fork keeps it.
	src = ForkSource{Origin: srcOrigin, ContentMD: "![[" + ownID + "]]\n> looks like\nAfter, mine.",
		ContentHTML: BakePartial(&Target{ID: ownID, Version: 3}, "looks like") + "\n<p>After, mine.</p>\n"}
	md, exact = FlattenFork(src, quoteLink)
	if !exact || !strings.HasSuffix(md, "\n\nAfter, mine.") {
		t.Fatalf("author's prose lost from a pre-#76 quote: %q exact=%v", md, exact)
	}
}

func TestFlattenForkReadsPartialityFromTheBake(t *testing.T) {
	// A client that predates partial quotes baked the whole item, so the >
	// lines after its directive are the author's prose (decision #62).
	src := ForkSource{Origin: srcOrigin, ContentMD: "![[" + ownID + "]]\n> my own aside",
		ContentHTML: `<blockquote class="blyg-transclusion" data-blyg-id="` + ownID + `" data-blyg-version="1"><p>whole</p></blockquote>` +
			"\n<blockquote><p>my own aside</p></blockquote>"}
	md, exact := FlattenFork(src, quoteLink)
	want := "> whole\n>\n> — quoted from [ex.test · v1](https://ex.test/me/items/" + ownID + ".json)\n\n> my own aside"
	if !exact || md != want {
		t.Fatalf("fork: %q exact=%v\nwant %q", md, exact, want)
	}
}

func TestFlattenForkKeepsDisclosures(t *testing.T) {
	src := pinned(t, "Mine [TK]impyrt=pasted[/TK] and [TK]x[=]made[/TK].", GenMeta{}, GenMeta{Model: "m-1"})
	md, exact := FlattenFork(src, quoteLink)
	if !exact || md != "Mine [TK]impyrt=pasted[/TK] and [TK]impyrt m-1=made[/TK]." {
		t.Fatalf("fork: %q exact=%v", md, exact)
	}
	c := compose(t, md, false)
	clean(t, c)
	if len(c.Generated) != 2 || c.Generated[1].Model != "m-1" || strings.Count(c.ContentHTML, "blyg-tk-gen") != 2 {
		t.Fatalf("a fork must not launder a disclosure (§5.7 rule 7): %+v %s", c.Generated, c.ContentHTML)
	}
}

func TestFlattenForkFallsBackToHTML(t *testing.T) {
	// The markdown names a quote the bake does not hold: rebuild from HTML,
	// which still flattens every quote it does hold.
	src := ForkSource{Origin: srcOrigin, ContentMD: "![[" + ownID + "]]\n\n![[" + remoteID + "]]",
		ContentHTML: `<blockquote class="blyg-transclusion" data-blyg-id="` + ownID + `" data-blyg-version="1">` +
			`<p>outer</p><blockquote class="blyg-transclusion" data-blyg-id="` + remoteID + `" data-blyg-version="2" data-blyg-origin="https://pi.test/"><p>inner <span class="blyg-tk-gen">gen</span></p></blockquote></blockquote>`}
	md, exact := FlattenFork(src, quoteLink)
	if exact {
		t.Fatal("mismatched bake must fall back")
	}
	want := "> outer\n>\n> > inner [TK]impyrt=gen[/TK]\n> >\n> > — quoted from [pi.test · v2](https://pi.test/items/" + remoteID + ".json)\n>\n" +
		"> — quoted from [ex.test · v1](https://ex.test/me/items/" + ownID + ".json)\n"
	if md != want {
		t.Fatalf("fallback:\n%s\nwant:\n%s", md, want)
	}
}
