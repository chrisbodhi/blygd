package studio

import (
	"context"
	"testing"

	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/testblyg"
)

// A flattened quote of an origin we hold nothing from links its item
// document where that origin's `item` template puts it (§5.6 rule 6,
// §6.1), reading the manifest once however often the origin is quoted.
func TestQuoteLinkFollowsTheItemTemplate(t *testing.T) {
	const id = "0a00000000000000000000000a"
	b := &testblyg.Blyg{S: testblyg.New(t), Mount: "/b/", Title: "T", ItemTmpl: "api?route=/items/{id}"}
	b.Publish()
	s := &Studio{Fetch: &fetch.Client{AllowPrivate: true}}
	link := s.quoteLink(context.Background(), &Origin{BaseURL: "https://own.example/"})
	for range 3 {
		if got := link(b.Origin(), id); got != b.Origin()+"api?route=/items/"+id {
			t.Fatalf("link %s", got)
		}
	}
	if n := b.S.Hits("/b/blyg.json"); n != 1 {
		t.Errorf("read the manifest %d times", n)
	}
}
