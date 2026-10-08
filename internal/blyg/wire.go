package blyg

import (
	"bytes"
	"encoding/json"
	"time"
)

// Time formats a protocol timestamp: ISO 8601 UTC at second precision (§4).
func Time(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// RFC822 formats an RSS date (§7). RSS 2.0 dates are RFC 822; GMT is the
// spelling every reader accepts.
func RFC822(t time.Time) string { return t.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") }

// Reference is the citation shape of §5.9. Only Origin, ID and Version are
// identity; Cited and Selector ride along verbatim and are never compared.
type Reference struct {
	Origin   string          `json:"origin,omitempty"`
	ID       string          `json:"id"`
	Version  int             `json:"version"`
	Selector json.RawMessage `json:"selector,omitempty"`
	Cited    json.RawMessage `json:"cited,omitempty"`
}

// ChangelogEntry is metadata about one version, never content (§5.2).
type ChangelogEntry struct {
	Version   int     `json:"version"`
	At        string  `json:"at"`
	Note      *string `json:"note"`
	Pinned    bool    `json:"pinned,omitempty"`
	Generated bool    `json:"generated,omitempty"`
}

// Media is one entry of an item's media array (§5.4).
type Media struct {
	URL  string `json:"url"`
	Mime string `json:"mime"`
	Alt  string `json:"alt"`
}

// ItemDoc is items/{id}.json (§5). Field order follows the spec's example
// and the reference client, so documents diff cleanly against theirs.
type ItemDoc struct {
	Blyg          string           `json:"blyg"`
	ID            string           `json:"id"`
	Kind          string           `json:"kind"`
	Origin        string           `json:"origin"`
	Page          string           `json:"page,omitempty"`
	Author        json.RawMessage  `json:"author,omitempty"`
	Created       string           `json:"created"`
	Updated       string           `json:"updated"`
	Version       int              `json:"version"`
	ContentMD     string           `json:"content_md"`
	ContentHTML   string           `json:"content_html"`
	ContentHash   string           `json:"content_hash"`
	Media         []Media          `json:"media"`
	Transclusions *[]Reference     `json:"transclusions,omitempty"` // threads always carry it (§10.3)
	StubOf        json.RawMessage  `json:"stub_of,omitempty"`
	ForkedFrom    json.RawMessage  `json:"forked_from,omitempty"`
	Generated     json.RawMessage  `json:"generated,omitempty"`
	Changelog     []ChangelogEntry `json:"changelog"`
}

// PinDoc is items/{id}/v{n}.json (§8).
type PinDoc struct {
	Blyg          string          `json:"blyg"`
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	Version       int             `json:"version"`
	At            string          `json:"at"`
	Note          *string         `json:"note"`
	Pinned        bool            `json:"pinned"`
	Origin        string          `json:"origin"`
	Author        json.RawMessage `json:"author,omitempty"`
	ContentMD     string          `json:"content_md"`
	ContentHTML   string          `json:"content_html"`
	ContentHash   string          `json:"content_hash"`
	Transclusions *[]Reference    `json:"transclusions,omitempty"`
	StubOf        json.RawMessage `json:"stub_of,omitempty"`
	ForkedFrom    json.RawMessage `json:"forked_from,omitempty"`
	Generated     json.RawMessage `json:"generated,omitempty"`
}

// Manifest is blyg.json (§6.1).
type Manifest struct {
	Blyg         string          `json:"blyg"`
	Level        int             `json:"level"`
	Generator    string          `json:"generator"`
	GeneratorURL string          `json:"generator_url,omitempty"`
	Site         string          `json:"site"`
	Title        string          `json:"title"`
	Author       json.RawMessage `json:"author,omitempty"`
	Feed         string          `json:"feed"`
	Items        string          `json:"items"`
	Blogroll     string          `json:"blogroll,omitempty"`
	Webmention   string          `json:"webmention,omitempty"`
	Updated      string          `json:"updated"`
}

// IndexEntry is one row of items/index.json (§6.2).
type IndexEntry struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Created string `json:"created"`
	Updated string `json:"updated"`
	Version int    `json:"version"`
}

// Index is items/index.json: every item ever published, withdrawn
// included, ordered by updated descending (§6.2).
type Index struct {
	Updated string       `json:"updated"`
	Items   []IndexEntry `json:"items"`
}

// Marshal encodes v as indented JSON without Go's default HTML escaping, so
// content_html travels as the bytes the author published rather than as
// <-escaped text. The output ends with a newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
