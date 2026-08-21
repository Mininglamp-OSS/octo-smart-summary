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

// fenceIgnorableClass matches runes that can be inserted INSIDE a tag name to break
// the literal rune run that structural matching depends on, without changing how a
// model reads the token:
//
//   - zero-width runes — format controls (Cf), non-spacing marks (Mn: variation
//     selectors U+FE00–FE0F and U+E0100+, the combining grapheme joiner U+034F,
//     combining accents), enclosing marks (Me), and the soft hyphen;
//   - SEPARATORS — whitespace (Z: space, U+3000 ideographic space, line/paragraph
//     separators) and C0/C1 controls (Cc, which is what carries a bare `\n`).
//
// The separator half closes the round-11 finding: `</文 档数据>`, `</文\u3000档数据>`
// and `</文\n档数据>` used to reach the model verbatim, and the last of those is
// reachable without any effort at all, because chunks are joined with `\n` — an
// attacker only has to split the tag across a chunk boundary. These tag names are
// CJK, which has no word spacing, so a separator between two of their runes is not
// a token boundary the way it would be in Latin prose: `</文 档数据>` reads as the
// same closing fence. Matching MORE here is the safe direction; the cost is that a
// body which spells the tag name out with separators between every rune (a table
// row `| 文 | 档 | 数 | 据 |`) is neutralized when a `<` precedes it.
//
// This class is applied ONLY between the runes of a tag name, never globally.
// A global \p{Mn} strip would corrupt legitimate decomposed text elsewhere in the
// document (café, Việt) — the document body is content to be summarized, not
// syntax, so it must survive byte-identical.
//
// Deliberate residual: \p{Mc} (spacing combining marks, e.g. U+0903) is NOT here.
// Those marks render visibly, so `</文ः档数据>` is a visibly different tag name, the
// same residual class as `<文档数据格式说明>`. Kept as a decision, not an oversight.
const fenceIgnorableClass = `[\p{Cf}\p{Mn}\p{Me}\p{Z}\p{Cc}\x{00AD}]*`

// fenceGlobalInvisiblePattern is the pre-pass strip. It stays limited to Cf and the
// soft hyphen for the reason above: those have no legitimate rendering role in
// prose, whereas Mn/Me do.
//
// U+200D ZERO WIDTH JOINER is carved out (see fenceGlobalInvisibleKeep): it is the
// one Cf rune with a load-bearing rendering role — stripping it globally split
// family emoji into their components and broke Devanagari conjuncts in the text
// being summarized. Carving it out costs no containment, because the tag-name
// ignorable class (which includes all of Cf) still neutralizes `</\u200d\u6587\u6863\u6570\u636e>`.
var fenceGlobalInvisiblePattern = regexp.MustCompile(`[\p{Cf}\x{00AD}]`)

const fenceGlobalInvisibleKeep = "\u200d"

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
	// Round 11 shipped `(?:[^>]*[^\p{L}\p{Nd}>])?` and got the direction wrong in the
	// other axis: `[^>]*` crosses arbitrary prose, newlines included, so a single `<`
	// anywhere in a body swallowed everything up to the next mention of the tag name
	// and replaced it with a 4–6 rune placeholder, with no truncation marker.
	// `当 x < y 时，文档数据 会被丢弃。` lost its middle; a 41-section document lost 39
	// sections; the same regression landed on the already-shipped <引用数据> path.
	// Over-neutralizing is the safe direction for injection but NOT for a summarizer:
	// it silently corrupts the very text the product exists to summarize.
	//
	// Two rules keep both failure modes closed at once:
	//
	//  1. The prefix carries no bare word run. It may cross a word run ONLY when that
	//     run is terminated by a solidus, i.e. markup shape rather than prose
	//     (`<0/文档数据>`, found by fuzzing in round 10). That is a rule, not a count,
	//     so unlike round 6's `{0,64}` tail there is nothing for an attacker to pad past,
	//     and unlike round 11's `[^>]*` it cannot walk into ordinary prose: `<` followed
	//     by `y 时，` never reaches the tag name, because `y` is not closed by a `/`.
	//  2. What the guard may therefore delete is bounded to the tag's OWN syntactic
	//     prefix — delimiter noise plus solidus-terminated tokens directly in front of
	//     the tag name, as in `<//文档数据>` or `<0/文档数据>`. It can no longer reach
	//     the sentence, paragraph, or section around the tag, which is the property
	//     round 11 lost. `TestFenceGuardDeletionIsBounded` pins it.
	//
	// Adjacency still decides token identity, which is what keeps prose intact:
	// `<0文档数据` stays prose (`0` continues the name), while `<0/文档数据>` is a tag.
	// Excluding `>` throughout stops a stray `<` reaching across an already-closed tag.
	const fenceDelimNoise = `[^` + fenceWordClass + `>]*`
	const fencePrefix = `(?:` + fenceDelimNoise + `[` + fenceWordClass + `]+/)*` + fenceDelimNoise

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
	return fenceGlobalInvisiblePattern.ReplaceAllStringFunc(s, func(m string) string {
		if m == fenceGlobalInvisibleKeep {
			return m
		}
		return ""
	})
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
//
// Both passes run to a FIXPOINT. Preserving the boundary rune is what keeps prose
// intact, but the boundary rune can itself be a `<`, and then the replacement sits
// next to it as a fresh tag: FuzzFenceGuard found `<文档数据<文档数据`, where one pass
// leaves `文档数据<文档数据` — an intact tag, and a violation of idempotence too. Looping
// is the structural answer rather than another pattern edit: it terminates because
// every rewrite strictly shortens the text (the same invariant the prompt budget
// already depends on), and it makes idempotence hold by construction instead of by
// argument.
func (g *fenceGuard) neutralize(s string) string {
	return strings.TrimSpace(g.rewriteToFixpoint(g.normalize(s), g.placeholder, g.headPlaceholder))
}

// strip removes fence tags OUTRIGHT. Used only to measure whether a document has
// any real content left; it must not be used on model-bound text, where the
// non-empty placeholder of neutralize is what preserves the injection's visibility.
func (g *fenceGuard) strip(s string) string {
	return strings.TrimSpace(g.rewriteToFixpoint(g.normalize(s), "", ""))
}

func (g *fenceGuard) rewriteToFixpoint(s, tagReplacement, headReplacement string) string {
	// Each iteration removes at least the leading `<` of some match, so the rune count
	// bounds the iteration count; the cap is belt-and-braces against a future edit that
	// breaks the shortening property, and is never reached today.
	for i := 0; i <= len(s); i++ {
		if !strings.ContainsRune(s, '<') {
			return s
		}
		next := g.tagPattern.ReplaceAllString(s, tagReplacement)
		// Head pass runs second, over whatever pass 1 declined, so ordinary well-formed
		// tags still render as the nicer [tagName].
		next = g.headPattern.ReplaceAllString(next, headReplacement+"${1}")
		if next == s {
			return s
		}
		s = next
	}
	return s
}
