package config

import (
	"slices"
	"strings"
	"testing"
)

func TestParseLinks(t *testing.T) {
	got, err := parseLinks(" Mastodon = https://social.example/@me ,, GitHub=https://github.com/me?tab=repos ")
	want := []Link{{"Mastodon", "https://social.example/@me"}, {"GitHub", "https://github.com/me?tab=repos"}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("parseLinks = %v, %v; want %v", got, err, want)
	}
	if got, err := parseLinks(""); err != nil || got != nil {
		t.Errorf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{"https://no-label.example/", "=https://ex.com/", "Me=/relative", "Me=javascript:alert(1)", "Me=mailto:me@ex.com", "Me=https://"} {
		if _, err := parseLinks(bad); err == nil {
			t.Errorf("parseLinks(%q) accepted", bad)
		}
	}
}

func TestParseName(t *testing.T) {
	if got, err := parseName("  Eurypos "); err != nil || got != "Eurypos" {
		t.Errorf("parseName = %q, %v", got, err)
	}
	for _, bad := range []string{"", "   ", "two\nlines", "tab\there", strings.Repeat("é", 61)} {
		if _, err := parseName(bad); err == nil {
			t.Errorf("parseName(%q) accepted", bad)
		}
	}
}
