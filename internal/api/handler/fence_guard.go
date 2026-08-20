package handler

import (
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Centralized data-fence guard.
//
// Rounds 4–10 of #201 each widened one hand-written pattern and each left a new
// bypass reachable, in two alternating dimensions:
//
//   - the GRAMMAR dimension (what tail shapes count as a tag) — closed in round 8
//     by neutralizing the tag *head* with a negative boundary class, so no bound
//     on the tail is required at all;
//   - the ALPHABET dimension (which runes count as `<`, `/`, or "invisible") —
//     which was still a hand-written enumeration and is what round 10 broke, with
//     eleven confirmed forms reaching the model verbatim.
//
// The lesson both reviewers drew is the same: a set the author enumerates by hand
// is a set the attacker picks from the complement. So neither dimension is
// enumerated here any more.
//
//   - Delimiters come from fenceDelimiterFolds, DERIVED from Unicode properties by
//     gen_fence_delims.go (NFKC folding to `<`/`>`/`/`, plus characters whose
//     Unicode NAME makes them angle-bracket or solidus homoglyphs). Regenerate with
//     `go generate ./internal/api/handler`; do not hand-edit the table.
//   - Invisibles come from a general-category CLASS (Cf/Mn/Me/soft-hyphen), not a
//     list of codepoints, so variation selectors, combining marks, and the
//     not-yet-assigned members of those categories are covered by construction.
//
// Both guards in this package (<文档数据> and <引用数据>) are built from this one
// implementation, so a fix can no longer land on one and miss the other — that
// divergence was itself a finding in round 9.
//
// Known residual, deliberately out of scope at this layer: delimiters the model
// decodes but this pass never sees as `<`, i.e. entity/escape encodings
// (`&lt;/文档数据&gt;`, `\u003c`). Those are a decoding concern for whoever
// introduces a decoder upstream of the prompt builder; no amount of widening here
// addresses them. FuzzFenceGuard pins the properties this layer does claim.
// ---------------------------------------------------------------------------

//go:generate go run gen_fence_delims.go

// fenceWordClass is what counts as "continues the tag name into a different
// token". It is deliberately `\p{L}\p{Nd}` rather than `\p{L}\p{N}`: \p{N} also
// contains No/Nl (parenthesized and circled numerals such as U+2478 ⑸, roman
// numerals), which render as distinct decorative glyphs rather than as digits
// continuing a word. Fuzzing found `<⑸文档数据` shipping verbatim under the wider
// class. Narrowing it makes the guard neutralize MORE, which is the safe
// direction, and keeps this rule identical to the test oracle's
// unicode.IsLetter/unicode.IsDigit pair.
const fenceWordClass = `\p{L}\p{Nd}`

// fenceIgnorableClass matches runes that occupy no visual width and can therefore
// be inserted INSIDE a tag name to break the literal rune run that structural
// matching depends on: format controls (Cf — zero-width space/joiner), non-spacing
// marks (Mn — variation selectors U+FE00–FE0F and U+E0100+, the combining grapheme
// joiner U+034F, combining accents), enclosing marks (Me), and the soft hyphen.
//
// This class is applied ONLY between the runes of a tag name, never globally.
// A global \p{Mn} strip would corrupt legitimate decomposed text elsewhere in the
// document (café, Việt) — the document body is content to be summarized, not
// syntax, so it must survive byte-identical.
const fenceIgnorableClass = `[\p{Cf}\p{Mn}\p{Me}\x{00AD}]*`

// fenceGlobalInvisiblePattern is the pre-pass strip. It stays limited to Cf and the
// soft hyphen for the reason above: those have no legitimate rendering role in
// prose, whereas Mn/Me do.
var fenceGlobalInvisiblePattern = regexp.MustCompile(`[\p{Cf}\x{00AD}]`)

// fenceControlFolds collapse separators that would otherwise let a tag straddle a
// line/record boundary invisibly.
var fenceControlFolds = []string{
	"\r", " ", "\t", " ", "\x00", " ", "\v", " ", "\f", " ",
	"\u0085", " ", "\u2028", " ", "\u2029", " ",
}

var fenceDelimiterReplacer = strings.NewReplacer(
	append(append([]string{}, fenceDelimiterFolds...), fenceControlFolds...)...,
)

// fenceGuard neutralizes forged fence tags for exactly one tag name.
type fenceGuard struct {
	tagName string
	// tagPattern matches a well-formed tag (optional attribute/self-closing tail).
	tagPattern *regexp.Regexp
	// headPattern matches the tag HEAD alone, with no closing `>` required. This is
	// what makes the guard convergent: once the leading `<` is gone the remainder is
	// inert text whatever its shape, so overlong, unclosed, and not-yet-imagined
	// tails all collapse together instead of one per review round.
	headPattern *regexp.Regexp
	// placeholder is 2 runes longer than tagName ("[" + tagName + "]"), matching the
	// 2 delimiter runes of the shortest full tag `<tagName>` exactly.
	placeholder string
	// headPlaceholder is the bare tag name: exactly 1 rune shorter than the shortest
	// head match `<tagName`, with the boundary rune preserved via ${1}.
	headPlaceholder string
}

func newFenceGuard(tagName string) *fenceGuard {
	// Interleave the ignorable class around every rune of the tag name, including a
	// leading position so `<\uFE0F文档数据>` is covered too.
	var name strings.Builder
	name.WriteString(fenceIgnorableClass)
	for _, r := range tagName {
		name.WriteString(regexp.QuoteMeta(string(r)))
		name.WriteString(fenceIgnorableClass)
	}
	n := name.String()

	// fencePrefix is the noise tolerated between `<` and the tag name. Like the tail
	// boundary, it is NEGATIVE and adjacency-based, and for the same reason: round 8
	// replaced the tail allow-list after `</文档数据">` escaped it, but left this side
	// as the allow-list `[\s\p{Zs}]*/*[\s\p{Zs}]*`. Fuzzing found the mirror bypass —
	// `<"文档数据>`, `<.文档数据>`, `<!文档数据>`, `<:文档数据>` all shipped verbatim.
	//
	// The prefix is either empty or ends in a non-letter/digit. That is the same
	// different-token rule the tail uses, applied to the other side: what decides
	// whether the name is its own token is the rune ADJACENT to it, not whether some
	// digit appears earlier. So `<0文档数据` stays prose (the name continues `0`),
	// while `<0/文档数据>` is a tag (the name is a separate token after `/`).
	// Excluding `>` throughout stops a stray `<` reaching across an already-closed tag.
	const fencePrefix = `(?:[^>]*[^` + fenceWordClass + `>])?`

	return &fenceGuard{
		tagName: tagName,
		// The optional tail must START with whitespace or a solidus, i.e. real
		// attribute/self-closing syntax, so prose that merely contains the tag name
		// (`<文档数据格式说明>`) is left alone. Anything this declines is still caught by
		// headPattern, so declining costs no containment.
		tagPattern: regexp.MustCompile(`<` + fencePrefix + n + `(?:[\s\p{Zs}/][^>]{0,64})?>`),
		// The boundary condition is deliberately NEGATIVE — "the tag name is not
		// continued by another letter or digit" — rather than an allow-list of
		// delimiters. An allow-list is a bound the attacker picks: round 8 briefly used
		// `[\s\p{Zs}/>]` and `</文档数据">` (one punctuation rune) matched neither pass
		// and shipped verbatim. Since these tag names are CJK, the only thing that makes
		// `<文档数据…` a different TOKEN is a letter/digit continuation.
		headPattern:     regexp.MustCompile(`<` + fencePrefix + n + `([^` + fenceWordClass + `]|$)`),
		placeholder:     "[" + tagName + "]",
		headPlaceholder: tagName,
	}
}

// normalize folds delimiter homoglyphs and strips globally-safe invisibles. Split
// out so neutralize and strip share exactly one normalization path.
func (g *fenceGuard) normalize(s string) string {
	s = fenceDelimiterReplacer.Replace(s)
	return fenceGlobalInvisiblePattern.ReplaceAllString(s, "")
}

// neutralize replaces forged fence tags with a NON-EMPTY placeholder.
//
// A non-empty placeholder is load-bearing twice over: it keeps the injection
// visible to the model as neutralized text, and it prevents split-token reassembly
// where deleting a tag splices its neighbours into a fresh copy of the same token.
//
// Budget invariant relied on by callers that pre-compute a rune budget: this can
// only ever SHORTEN the text. Pass 1's shortest match `<tagName>` is 2 runes longer
// than tagName and maps to a placeholder exactly 2 runes longer than tagName;
// pass 2's shortest match `<tagName` is 1 rune longer and maps to tagName itself
// with the boundary rune preserved. Ignorables and folded delimiters only ever make
// a match longer while the replacement stays fixed.
func (g *fenceGuard) neutralize(s string) string {
	s = g.normalize(s)
	s = g.tagPattern.ReplaceAllString(s, g.placeholder)
	// Head pass runs second, over whatever pass 1 declined, so ordinary well-formed
	// tags still render as the nicer [tagName].
	s = g.headPattern.ReplaceAllString(s, g.headPlaceholder+"${1}")
	return strings.TrimSpace(s)
}

// strip removes fence tags OUTRIGHT. Used only to measure whether a document has
// any real content left; it must not be used on model-bound text, where the
// non-empty placeholder of neutralize is what preserves the injection's visibility.
func (g *fenceGuard) strip(s string) string {
	s = g.normalize(s)
	s = g.tagPattern.ReplaceAllString(s, "")
	return strings.TrimSpace(g.headPattern.ReplaceAllString(s, "${1}"))
}
