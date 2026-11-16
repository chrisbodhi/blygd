package blyg

import (
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
)

// Unicode on the wire (§5.1, §5.3, §14; revision of 2026-11-12, decision
// #103). Every check here needs only what a standard library offers, as §3
// asks of a reader or receiver; NFC and grapheme clusters are publisher
// quality, and use the libraries publishing already has.

// TextFault says why content_md is not text, or "" when it is (§5.1): it
// MUST NOT contain a C0 control other than tab, line feed and carriage
// return, or an unpaired surrogate. Neither can appear in well-formed XML,
// so one would make the whole feed.xml unparseable for a strict reader. A
// Go string holds UTF-8, where a surrogate is invalid, so invalid UTF-8 is
// a fault too. A publisher also refuses PublishFault's noncharacters.
func TextFault(s string) string { return textFault(s, false) }

// PublishFault is TextFault plus U+FFFE and U+FFFF, which a publisher
// SHOULD NOT publish (§5.1) and blygd refuses. A reader does not reject
// them: they are no MUST.
func PublishFault(s string) string { return textFault(s, true) }

func textFault(s string, nonchars bool) string {
	for i, r := range s {
		switch {
		case r == utf8.RuneError && !validAt(s, i):
			return fmt.Sprintf("invalid UTF-8 at byte %d", i)
		case r < 0x20 && r != '\t' && r != '\n' && r != '\r':
			return fmt.Sprintf("the control character U+%04X at byte %d", r, i)
		case nonchars && (r == 0xFFFE || r == 0xFFFF):
			return fmt.Sprintf("the noncharacter U+%04X at byte %d", r, i)
		}
	}
	return ""
}

func validAt(s string, i int) bool {
	_, size := utf8.DecodeRuneInString(s[i:])
	return size == 3 // an encoded U+FFFD is three bytes; an invalid byte is one
}

// LoneSurrogate reports whether a JSON string literal, quotes included,
// escapes a surrogate that is not half of a pair (\uD800 alone, or a low
// half first). encoding/json would read either as U+FFFD, so a reader
// checks the bytes it was served (§5.1, §13.2).
func LoneSurrogate(raw []byte) bool {
	high := false // the last thing read was a high surrogate's escape
	for i := 0; i < len(raw); {
		if raw[i] == '\\' && i+1 < len(raw) {
			if raw[i+1] == 'u' && i+6 <= len(raw) {
				if v, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 32); err == nil {
					lo := v >= 0xDC00 && v <= 0xDFFF
					if high != lo {
						return true
					}
					high = v >= 0xD800 && v <= 0xDBFF
					i += 6
					continue
				}
			}
			if high {
				return true
			}
			i += 2 // an escaped character never starts another escape
			continue
		}
		if high {
			return true
		}
		i++
	}
	return high
}

// IsNFC reports whether s is in Unicode Normalization Form C, which a
// publisher SHOULD publish content_md in (§5.1). Readers never normalize.
func IsNFC(s string) bool { return norm.NFC.IsNormalString(s) }

// bidiControls are the embedding, override and isolate controls (§14).
func isBidiOpen(r rune) bool {
	return (r >= 0x202A && r <= 0x202E && r != 0x202C) || (r >= 0x2066 && r <= 0x2068)
}
func isBidiClose(r rune) bool { return r == 0x202C || r == 0x2069 }

// UnpairedBidi reports whether s opens a bidirectional embedding, override
// or isolate it does not close, or closes one it never opened, within a
// line, which a publisher SHOULD NOT publish (§14): it reorders whatever
// stands next to the string once a reader shows it.
func UnpairedBidi(s string) bool {
	depth := 0
	for _, r := range s {
		switch {
		case r == '\n':
			if depth != 0 {
				return true
			}
		case isBidiOpen(r):
			depth++
		case isBidiClose(r):
			if depth == 0 {
				return true
			}
			depth--
		}
	}
	return depth != 0
}

// Truncate cuts s to at most n code points, which is how a cap stated in
// characters counts (§5.3). It never yields an unpaired surrogate (a Go
// string holds none) and does not split an extended grapheme cluster
// (UAX #29): the cut backs up to the last cluster boundary at or before n.
// ok reports whether anything was cut.
func Truncate(s string, n int) (out string, ok bool) {
	if n <= 0 {
		return "", s != ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	count, end := 0, 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		c := utf8.RuneCountInString(g.Str())
		if count+c > n {
			break
		}
		count += c
		_, end = g.Positions()
	}
	return s[:end], true
}

// Clip holds s to at most n code points, an ellipsis included when it cuts,
// without splitting a grapheme cluster (§5.3).
func Clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	cut, _ := Truncate(s, n-1)
	return cut + "…"
}
