package blyg

import (
	"regexp"
	"strings"
)

// The TK authoring grammar: studio-private markup for generated spans,
// shared with the reference client so drafts move between clients. None of
// it reaches the wire (§5.7 rule 2): publish strips every scope to its bare
// output, wraps that output in blyg-tk-gen, and discloses it in generated[].
//
//	[TK]instruction[=]output[/TK]      generated from the instruction; the
//	                                   ![[id]]s in it are the sources
//	[TK]impyrt=text[/TK]               generated elsewhere (§5.7 rule 7)
//	[TK]impyrt <model>=text[/TK]       … by a known model
//
// blygd runs no model, so a scope is the client's declaration that its
// output is machine-generated, and every scope is disclosed.

var (
	impyrtInline      = regexp.MustCompile(`(?i)^\s*impyrt(?:[ \t]+([^\s=]+))?[ \t]*=`)
	impyrtInstruction = regexp.MustCompile(`(?i)^impyrt(?:[ \t]+(\S+))?$`)
	blankBefore       = regexp.MustCompile(`(^\s*|\n[ \t]*\n)[ \t]*$`)
	blankAfter        = regexp.MustCompile(`^[ \t]*(\n[ \t]*\n|\s*$)`)
)

// tkScope is one parsed scope.
type tkScope struct {
	start, end  int // the whole scope, "[TK]" through "[/TK]"
	instruction string
	output      string
	hasOutput   bool
	sourceIDs   []string // the instruction's ![[id]]s, deduplicated, in order
	block       bool     // alone in its paragraph: renders as a <div>
	imported    bool
	model       string // impyrt's model, when written
}

// parseScopes scans for [TK] … [/TK] with no nesting and no bracket
// balancing, so ![[id]] is safe inside either half. A [TK] inside code is
// an example of the grammar, not a scope.
func parseScopes(md string) ([]tkScope, []string) {
	var scopes []tkScope
	var errs []string
	var code []span
	if strings.Contains(md, "[TK]") {
		code = codeSpans(md)
	}
	for i := 0; ; {
		open := indexFrom(md, "[TK]", i)
		if open < 0 {
			break
		}
		if inSpans(code, open) {
			i = open + 4
			continue
		}
		closeAt := indexFrom(md, "[/TK]", open+4)
		if closeAt < 0 {
			errs = append(errs, "a [TK] scope is never closed with [/TK]")
			break
		}
		if nested := indexFrom(md, "[TK]", open+4); nested >= 0 && nested < closeAt {
			errs = append(errs, "[TK] scopes cannot nest")
			i = closeAt + 5
			continue
		}
		s := tkScope{start: open, end: closeAt + 5}
		eq := indexFrom(md, "[=]", open+4)
		hasEq := eq >= 0 && eq < closeAt
		instrEnd := closeAt
		if hasEq {
			instrEnd = eq
			s.output, s.hasOutput = md[eq+3:closeAt], true
		}
		s.instruction = strings.TrimSpace(md[open+4 : instrEnd])
		if !hasEq {
			if m := impyrtInline.FindStringSubmatchIndex(md[open+4 : closeAt]); m != nil {
				s.imported, s.hasOutput = true, true
				s.output = md[open+4+m[1] : closeAt]
				if m[2] >= 0 {
					s.model = md[open+4+m[2] : open+4+m[3]]
				}
			}
		} else if m := impyrtInstruction.FindStringSubmatch(s.instruction); m != nil {
			s.imported, s.model = true, m[1]
		}
		if !s.imported {
			seen := map[string]bool{}
			// A source is written as a directive, label allowed, anywhere
			// in the instruction.
			instr := md[open+4 : instrEnd]
			for _, m := range findLinks(instr) {
				if id := instr[m[2]:m[3]]; m[0] > 0 && instr[m[0]-1] == '!' && !seen[id] {
					seen[id] = true
					s.sourceIDs = append(s.sourceIDs, id)
				}
			}
		}
		s.block = blankBefore.MatchString(md[:s.start]) && blankAfter.MatchString(md[s.end:])
		scopes = append(scopes, s)
		i = s.end
	}
	return scopes, errs
}

func indexFrom(s, sub string, from int) int {
	if from > len(s) {
		return -1
	}
	if i := strings.Index(s[from:], sub); i >= 0 {
		return from + i
	}
	return -1
}

// genSpan is a scope's output located in the stripped text.
type genSpan struct {
	start, end int
	scope      int
}

// WireMD is the content_md a draft publishes as: its [TK] grammar removed,
// each scope's output kept (§5.7). ok is false when the scopes do not
// parse, or one has no output yet, so the draft cannot publish as it is.
func WireMD(draft string) (md string, ok bool) {
	scopes, errs := parseScopes(draft)
	if len(errs) > 0 {
		return "", false
	}
	for _, s := range scopes {
		if !s.hasOutput {
			return "", false
		}
	}
	md, _ = stripScopes(draft, scopes)
	return md, true
}

// stripScopes removes every scope's grammar and keeps its output in place:
// the wire content_md (§5.7: no authoring grammar reaches the wire).
func stripScopes(md string, scopes []tkScope) (string, []genSpan) {
	var b strings.Builder
	var spans []genSpan
	last := 0
	for i, s := range scopes {
		b.WriteString(md[last:s.start])
		at := b.Len()
		b.WriteString(s.output)
		spans = append(spans, genSpan{at, b.Len(), i})
		last = s.end
	}
	b.WriteString(md[last:])
	return b.String(), spans
}

// Private Use Area sentinels, never produced by authoring or by the
// renderer: substituted before rendering, spliced after. The same code
// points as the reference client.
const (
	blockSentinel = ""
	inlineOpen    = ""
	inlineClose   = ""
	linkSentinel  = ""
)

// pctEncoded is how the renderer writes a sentinel into a URL attribute.
func pctEncoded(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteString("%")
		b.WriteString(strings.ToUpper(hex2(s[i])))
	}
	return b.String()
}

func hex2(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&15]})
}
