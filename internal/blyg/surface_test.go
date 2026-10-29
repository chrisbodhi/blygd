package blyg

import "testing"

func TestSurfaceDefaultsAndTemplates(t *testing.T) {
	// Absent keys are the 0.3 paths (§4).
	d := SurfaceOf("https://ex.test/blyg/", []byte(`{"blyg":"0.4","title":"x"}`))
	for got, want := range map[string]string{
		d.FeedURL():        "https://ex.test/blyg/feed.xml",
		d.IndexURL():       "https://ex.test/blyg/items/index.json",
		d.ItemURL(ownID):   "https://ex.test/blyg/items/" + ownID + ".json",
		d.PinURL(ownID, 2): "https://ex.test/blyg/items/" + ownID + "/v2.json",
	} {
		if got != want {
			t.Errorf("default: %s, want %s", got, want)
		}
	}
	if d.Templated() {
		t.Error("a manifest without templates is not templated")
	}
	// A relocated surface, absolute and with query strings (§6.1).
	s := SurfaceOf("https://ex.test/wp-json/blyg/v1/", []byte(`{"feed":"https://ex.test/feed/blyg/",
		"items":"items","item":"https://ex.test/?rest_route=/blyg/v1/items/{id}","pin":"items/{id}/v{n}"}`))
	for got, want := range map[string]string{
		s.FeedURL():        "https://ex.test/feed/blyg/",
		s.IndexURL():       "https://ex.test/wp-json/blyg/v1/items",
		s.ItemURL(ownID):   "https://ex.test/?rest_route=/blyg/v1/items/" + ownID,
		s.PinURL(ownID, 3): "https://ex.test/wp-json/blyg/v1/items/" + ownID + "/v3",
	} {
		if got != want {
			t.Errorf("templated: %s, want %s", got, want)
		}
	}
	if !s.Templated() {
		t.Error("an item template relocates item documents")
	}
	// Level 1 escapes everything but unreserved characters.
	if got := expand("a/{id}/{x}", map[string]string{"id": "a b/ç"}); got != "a/a%20b%2F%C3%A7/" {
		t.Errorf("expand: %s", got)
	}
}

// What blygd writes (ItemPath, PinPath, the defaults) and what a reader
// fetches from a manifest that moves nothing must be the same paths (§4).
func TestDefaultSurfaceIsWhatWePublish(t *testing.T) {
	const origin, id = "https://example.com/blyg/", "0a00000000000000000000000a"
	s := Surface{Origin: origin}
	for _, c := range [][2]string{
		{s.ItemURL(id), origin + ItemPath(id)},
		{s.ItemURL(id), ItemDocURL(origin, id)},
		{s.PinURL(id, 3), origin + PinPath(id, 3)},
		{s.PinURL(id, 3), PinURL(origin, id, 3)},
		{s.FeedURL(), origin + DefaultFeed},
		{s.IndexURL(), origin + DefaultItems},
		{origin + ItemPath(id), origin + "items/" + id + ".json"},
		{origin + PinPath(id, 3), origin + "items/" + id + "/v3.json"},
	} {
		if c[0] != c[1] {
			t.Errorf("%s != %s", c[0], c[1])
		}
	}
}
