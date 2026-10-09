package web

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/mention"
)

func pngBytes(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.Gray{Y: shade})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (u *user) upload(body []byte, mime string) resp {
	return u.e.do("POST", u.e.srv.URL+"/api/media", u.token, body, "Content-Type", mime)
}

func TestMedia(t *testing.T) {
	e := newEnv(t, mention.DefaultRateLimits)
	alice := e.user("alice")
	ctx := context.Background()
	img := pngBytes(t, 10)

	r := alice.upload(img, "image/png")
	expect(t, r, 201)
	m := r.json(t)
	path := m["path"].(string)
	if !strings.HasPrefix(path, "media/") || !strings.HasSuffix(path, ".png") || len(path) != len("media/")+32+4 || m["published"] != false {
		t.Fatalf("upload: %s", r.body)
	}
	// Content addressing: the same bytes are the same object.
	if r = alice.upload(img, "image/png"); r.json(t)["path"] != path {
		t.Fatalf("re-upload changed path: %s", r.body)
	}
	expect(t, alice.upload(img, "image/jpeg"), 422) // declared type must match the bytes
	expect(t, alice.upload([]byte("<svg/>"), "image/svg+xml"), 422)
	e.studio.MediaMax = 10
	expect(t, alice.upload(img, "image/png"), 413)
	e.studio.MediaMax = 0

	// Until a published version references it, it is a draft's: 404.
	expect(t, alice.get(path), 404)

	id := alice.draft(t, map[string]any{"content_md": "See ![a diagram](" + path + ") and [the file](" + path + ")."})
	alice.publish(t, id, 200)
	d := alice.itemDoc(t, id)
	if len(d.Media) != 1 || d.Media[0] != (blyg.Media{URL: path, Mime: "image/png", Alt: "a diagram"}) {
		t.Fatalf("media array (§5.4): %+v", d.Media)
	}
	if !strings.Contains(d.ContentHTML, `src="`+alice.origin+path+`"`) {
		t.Fatalf("media/… resolves against the origin, not the page: %s", d.ContentHTML)
	}
	r = alice.get(path)
	expect(t, r, 200)
	if !bytes.Equal(r.body, img) || r.header.Get("Content-Type") != "image/png" ||
		!strings.Contains(r.header.Get("Cache-Control"), "immutable") || r.header.Get("Content-Security-Policy") == "" {
		t.Fatalf("served media: %v", r.header)
	}
	expect(t, alice.get(path, "If-None-Match", r.header.Get("ETag")), 304)

	// Published media is permanent: any version may be pinned later.
	expect(t, alice.api("DELETE", "/"+path, nil), 409)
	if _, err := e.pool.Exec(ctx, `DELETE FROM media WHERE path = $1`, path); err == nil {
		t.Error("the schema let referenced media be deleted")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE media SET bytes = 'x' WHERE path = $1`, path); err == nil {
		t.Error("the schema let media bytes change")
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM version_media WHERE path = $1`, path); err == nil {
		t.Error("the schema let a version's media reference be dropped")
	}

	// A reference to media that does not exist is a publish error.
	alice.edit(t, id, "![x](media/00000000000000000000000000000000.png)")
	if p := problems(t, alice.publish(t, id, 422)); !strings.Contains(p, "no such media") {
		t.Errorf("missing media: %s", p)
	}

	// Withdrawal empties the endcap's media array; the bytes stay served,
	// because v1 still references them and may yet be pinned.
	expect(t, alice.api("POST", "/items/"+id+"/withdraw", nil), 200)
	if d = alice.itemDoc(t, id); len(d.Media) != 0 {
		t.Errorf("endcap media: %+v", d.Media)
	}
	expect(t, alice.get(path), 200)

	// An upload no version used may be deleted.
	other := alice.upload(pngBytes(t, 200), "image/png").json(t)["path"].(string)
	expect(t, alice.api("DELETE", "/"+other, nil), 204)
	r = alice.api("GET", "/media", nil)
	expect(t, r, 200)
	if strings.Contains(string(r.body), other) || !strings.Contains(string(r.body), `"published": true`) {
		t.Errorf("media list: %s", r.body)
	}
}
