package surface

import (
	"context"
	"testing"

	"github.com/chrisbodhi/blygd/internal/fetch"
	"github.com/chrisbodhi/blygd/internal/testblyg"
)

const id = "0a00000000000000000000000a"

func TestLocate(t *testing.T) {
	ctx := context.Background()
	c := &fetch.Client{AllowPrivate: true}

	// A templated blyg: the manifest's keys win.
	b := &testblyg.Blyg{S: testblyg.New(t), Mount: "/b/", Title: "T",
		ItemTmpl: "api?route=/items/{id}", PinTmpl: "api?route=/items/{id}/v{n}"}
	b.Publish()
	l, err := Locate(ctx, c, b.Origin(), fetch.VerifyLimits)
	if err != nil || !l.Found() || l.Transient() {
		t.Fatalf("templated: %+v %v", l, err)
	}
	if got := l.ItemURL(id); got != b.Origin()+"api?route=/items/"+id {
		t.Errorf("item %s", got)
	}
	if got := l.PinURL(id, 3); got != b.Origin()+"api?route=/items/"+id+"/v3" {
		t.Errorf("pin %s", got)
	}

	// No manifest: the defaults, and nothing found.
	l, err = Locate(ctx, c, b.S.URL+"/nothing/", fetch.VerifyLimits)
	if err != nil || l.Found() || l.Transient() || l.Status != 404 || l.ItemURL(id) != b.S.URL+"/nothing/items/"+id+".json" {
		t.Errorf("absent: %+v %v", l, err)
	}

	// A 503 is transient; a body that is not a manifest is not found.
	b.S.SetResponse("/down/blyg.json", testblyg.Response{Status: 503})
	b.S.Set("/junk/blyg.json", `{"item": "x/{id}"}`)
	if l, _ := Locate(ctx, c, b.S.URL+"/down/", fetch.VerifyLimits); !l.Transient() {
		t.Errorf("503: %+v", l)
	}
	if l, _ := Locate(ctx, c, b.S.URL+"/junk/", fetch.VerifyLimits); l.Found() || l.Templated() {
		t.Errorf("not a manifest, yet its template was used: %+v", l)
	}

	// The cache reads each origin once.
	cache := &Cache{G: c, Lim: fetch.VerifyLimits}
	before := b.S.Hits("/b/blyg.json")
	for range 3 {
		if !cache.Locate(ctx, b.Origin()).Templated() {
			t.Fatal("cache lost the template")
		}
	}
	if n := b.S.Hits("/b/blyg.json") - before; n != 1 {
		t.Errorf("cache fetched the manifest %d times", n)
	}
}
