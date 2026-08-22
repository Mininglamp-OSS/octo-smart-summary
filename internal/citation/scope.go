// Package citation holds the vocabulary shared by every stage that reads or
// rewrites `[n]` citation markers inside a generated body.
//
// It exists because the repo has now had to answer the SAME question in three
// places — "is this bracketed token a citation marker, or is it ordinary
// content?" — and answering it twice is how the R11 Q5 defect happened: a
// strip that deleted every bracketed integer anywhere, including `items[0]`
// inside fenced code, `GB/T 7714 [2020]`, and the `[1]` out of `[1](url)`.
//
// Import direction: internal/api/handler already imports internal/worker
// (agent_summary_citations.go), so the worker cannot import the handler and the
// hardened helpers in handler/share.go cannot be reused upward. A leaf package
// depending on nothing but the standard library is the only shape that lets
// BOTH call sites share one definition instead of growing a second one.
package citation

import "strings"

// MarkerRewriter decides what happens to one bracketed token that has already
// passed the syntactic scoping rules below.
//
// token is the raw text between the brackets ("3", "P2", "2020", "+5"): the
// callers disagree about what a marker looks like (the handler matches an
// explicit `[n]`/`[Pn]` marker set, the worker parses a 1-based pool ordinal),
// so the SYNTAX is shared here and the SEMANTICS stay with the caller.
//
// Returning rewrite=false leaves the token byte-identical. That is the default
// for anything the caller cannot positively identify: an unrecognised `[2020]`
// in prose is correct content, and deleting it is data loss, whereas keeping it
// costs nothing.
type MarkerRewriter func(token string) (replacement string, rewrite bool)

// RewriteMarkers walks content and offers every syntactically-eligible
// bracketed token to fn, splicing in whatever fn asks for.
//
// Scoping rules — all of them NARROWING, all of them settled previously in
// handler.stripUnresolvedCitationMarkers (R11 Q5) and re-derived here once:
//
//   - fenced code regions (``` ... ```) are passed through untouched, so
//     `items[0] = x` in a Go block survives;
//   - a markdown inline link `[1](url)` is content, never a marker — deleting
//     or renumbering the `[1]` silently corrupts the link;
//   - a reference-style link `[1][docs]` is content for the same reason;
//   - a footnote definition `[1]: https://…` is content for the same reason;
//   - an unterminated `[` is copied verbatim rather than swallowing the tail.
//
// Everything else is offered to fn, which may still decline it.
//
// Whitespace around a removal is deliberately NOT touched here: the handler
// pins the surrounding spacing byte-for-byte, so tidying belongs to the caller
// that wants it.
func RewriteMarkers(content string, fn MarkerRewriter) string {
	if fn == nil {
		return content
	}
	lines := strings.Split(content, "\n")
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			lines[i] = rewriteMarkersInLine(line, fn)
		}
	}
	return strings.Join(lines, "\n")
}

// rewriteMarkersInLine applies RewriteMarkers' rules to a single non-fenced
// line. See RewriteMarkers for the scoping rules.
func rewriteMarkersInLine(line string, fn MarkerRewriter) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '[' {
			b.WriteByte(line[i])
			i++
			continue
		}
		end := strings.IndexByte(line[i:], ']')
		if end < 0 {
			// Unterminated bracket: emit the rest verbatim rather than
			// consuming it.
			b.WriteString(line[i:])
			break
		}
		end += i
		// A link or footnote definition is content, not a marker.
		if end+1 < len(line) {
			switch line[end+1] {
			case '(', ':':
				b.WriteString(line[i : end+1])
				i = end + 1
				continue
			case '[':
				// Reference-style link [1][docs]. BOTH brackets are part of the
				// link, so the LABEL is consumed here too — offering it to fn
				// would let a caller rewrite the `[2]` out of `[1][2]` and break
				// the reference, which is the same corruption the exemption
				// exists to prevent one bracket to the left.
				if labelEnd := strings.IndexByte(line[end+1:], ']'); labelEnd >= 0 {
					labelEnd += end + 1
					b.WriteString(line[i : labelEnd+1])
					i = labelEnd + 1
					continue
				}
				b.WriteString(line[i : end+1])
				i = end + 1
				continue
			}
		}
		if replacement, rewrite := fn(line[i+1 : end]); rewrite {
			b.WriteString(replacement)
			i = end + 1
			continue
		}
		b.WriteString(line[i : end+1])
		i = end + 1
	}
	return b.String()
}
