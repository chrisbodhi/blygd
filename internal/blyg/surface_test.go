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
