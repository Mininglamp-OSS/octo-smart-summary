// Package citationtext defines the message citation syntax shared by Agent and
// Workflow. Evidence ordinals stay unchanged; reading-order labels belong to UI.
package citationtext

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

const MaxGroupSize = 128
const maxExpansionBytes = 500 * 1024
const OutputRule = "多来源必须逐个写成 [9][73]，不得写成 [9,73] 或 [9-73]、[9–73]。只保留真正支持该事实的来源，不用范围缩写。"

var ErrInvalid = errors.New("invalid or unresolved citation")
var candidate = regexp.MustCompile(`\[[ \t]*[0-9][0-9 \t,，;；\-–—]*\]`)
var item = regexp.MustCompile(`^([0-9]+)(?:[ \t]*[-–—][ \t]*([0-9]+))?$`)
var dateShape = regexp.MustCompile(`^[0-9]{4}[-–—][0-9]{2}[-–—][0-9]{2}$`)
var definition = regexp.MustCompile(`(?m)^ {0,3}\[([^\]\n]+)\]:`)
var listStart = regexp.MustCompile(`^(?:[-+*]|[0-9]+[.)])[ \t]+`)

type Marker struct {
	Start, End int
	Indices    []int
	Compound   bool
}

// Scan ignores Markdown code, escaped brackets, images and link labels/targets.
// Numeric groups are bounded before expansion; malformed groups remain visible
// to validators as markers with no indices rather than silently disappearing.
func Scan(content string) []Marker {
	protected := protectedSpans(content)
	var out []Marker
	for _, loc := range candidate.FindAllStringIndex(content, -1) {
		start, end := loc[0], loc[1]
		if protected[start] || escaped(content, start) ||
			(start > 0 && content[start-1] == '!') ||
			(end < len(content) && (content[end] == '(' || content[end] == ':')) {
			continue
		}
		body := content[start+1 : end-1]
		if dateShape.MatchString(strings.TrimSpace(body)) {
			continue // A bracketed ISO-shaped date is prose, never evidence.
		}
		m := Marker{Start: start, End: end, Compound: strings.ContainsAny(body, " \t,，;；-–—")}
		for _, part := range strings.Split(strings.ReplaceAll(body, "，", ","), ",") {
			match := item.FindStringSubmatch(strings.TrimSpace(part))
			if match == nil {
				m.Indices = nil
				break
			}
			first, err := strconv.Atoi(match[1])
			last := first
			if match[2] != "" {
				last, err = strconv.Atoi(match[2])
			}
			if err != nil || first < 1 || first > 99999 || last < first || last > 99999 ||
				last-first+1 > MaxGroupSize-len(m.Indices) {
				m.Indices = nil
				break
			}
			for n := first; n <= last; n++ {
				m.Indices = append(m.Indices, n)
			}
		}
		out = append(out, m)
	}
	return out
}

// Canonicalize expands explicit lists/inclusive ranges only when every member
// is backed by the caller's authorized evidence. It is atomic on error.
// Single markers remain unchanged, allowing existing prose-number policies.
// maxIndex is the known numeric evidence window; -1 means unknown (fail closed).
func Canonicalize(content string, valid func(int) bool, maxIndex int) (string, error) {
	var b strings.Builder
	offset := 0
	for _, m := range Scan(content) {
		if !m.Compound {
			continue
		}
		if outsideEvidenceWindow(m, maxIndex) {
			continue
		}
		if len(m.Indices) == 0 {
			return content, ErrInvalid
		}
		for _, n := range m.Indices {
			if valid == nil || !valid(n) {
				return content, ErrInvalid
			}
		}
		b.WriteString(content[offset:m.Start])
		seen := make(map[int]bool)
		for _, n := range m.Indices {
			if !seen[n] {
				b.WriteString("[" + strconv.Itoa(n) + "]")
				seen[n] = true
			}
		}
		if b.Len() > len(content)+maxExpansionBytes {
			return content, ErrInvalid
		}
		offset = m.End
	}
	b.WriteString(content[offset:])
	if b.Len() > len(content)+maxExpansionBytes {
		return content, ErrInvalid
	}
	return b.String(), nil
}

func Valid(content string, valid func(int) bool, maxIndex int, requireMarker bool) bool {
	markers := Scan(content)
	hasCitation := false
	for _, m := range markers {
		if outsideEvidenceWindow(m, maxIndex) {
			continue
		}
		if len(m.Indices) == 0 {
			return false
		}
		for _, n := range m.Indices {
			if valid == nil || !valid(n) {
				return false
			}
		}
		hasCitation = true
	}
	return !requireMarker || hasCitation
}

// Only well-formed groups wholly above a KNOWN evidence window are prose.
// Use the window, not membership: [2,2] with evidence {1,3} is corruption.
// An unknown window cannot excuse a failed citation build. Malformed
// groups and expansion-limit violations also remain fail-closed.
func outsideEvidenceWindow(m Marker, maxIndex int) bool {
	if !m.Compound || maxIndex < 0 || len(m.Indices) == 0 {
		return false
	}
	for _, n := range m.Indices {
		if n <= maxIndex {
			return false
		}
	}
	return true
}

func escaped(s string, pos int) bool {
	n := 0
	for pos > 0 && s[pos-1] == '\\' {
		n++
		pos--
	}
	return n%2 == 1
}

// Only masking is needed: replacements use original byte offsets, preserving
// Markdown verbatim. Fences honor delimiter type/length, including unclosed ones.
func protectedSpans(s string) []bool {
	mask := make([]bool, len(s))
	mark := func(a, b int) {
		for i := a; i < b; i++ {
			mask[i] = true
		}
	}
	var fence byte
	fenceLen := 0
	inList := false
	previousBlank := true
	for pos := 0; pos < len(s); {
		end := strings.IndexByte(s[pos:], '\n')
		if end < 0 {
			end = len(s)
		} else {
			end += pos + 1
		}
		line := strings.TrimSpace(s[pos:end])
		indent := len(s[pos:end]) - len(strings.TrimLeft(s[pos:end], " \t"))
		if listStart.MatchString(line) {
			inList = true
		} else if line != "" && indent == 0 {
			inList = false
		}
		run := 0
		if len(line) > 0 && (line[0] == '`' || line[0] == '~') {
			for run < len(line) && line[run] == line[0] {
				run++
			}
		}
		if fence != 0 {
			mark(pos, end)
			if run >= fenceLen && line[0] == fence && strings.TrimSpace(line[run:]) == "" {
				fence = 0
			}
		} else if run >= 3 {
			fence, fenceLen = line[0], run
			mark(pos, end)
		} else if !inList && (previousBlank || (pos > 0 && mask[pos-1])) &&
			(strings.HasPrefix(s[pos:end], "    ") || strings.HasPrefix(s[pos:end], "\t")) {
			mark(pos, end)
		}
		previousBlank = line == ""
		pos = end
	}
	for i := 0; i < len(s); i++ {
		if mask[i] || s[i] != '`' || escaped(s, i) {
			continue
		}
		n := 1
		for i+n < len(s) && s[i+n] == '`' {
			n++
		}
		for j := i + n; j < len(s); j++ {
			if mask[j] || s[j] != '`' {
				continue
			}
			k := j
			for k < len(s) && s[k] == '`' {
				k++
			}
			if k-j == n {
				mark(i, k)
				i = k - 1
				break
			}
			j = k - 1
		}
	}
	// Protect inline/reference link labels and destinations, including numeric
	// brackets embedded inside a label. Adjacent [9][73] are citation clusters,
	// not links unless a numeric reference definition exists in the document.
	defs := make(map[string]bool)
	for _, d := range definition.FindAllStringSubmatch(s, -1) {
		defs[strings.ToLower(d[1])] = true
	}
	var opens []int
	for i := 0; i < len(s); i++ {
		if mask[i] || escaped(s, i) {
			continue
		}
		if s[i] == '[' {
			opens = append(opens, i)
			continue
		}
		if s[i] != ']' || len(opens) == 0 {
			continue
		}
		start := opens[len(opens)-1]
		opens = opens[:len(opens)-1]
		j := i + 1
		if j < len(s) && s[j] == '(' {
			k, nesting := j+1, 1
			for ; k < len(s) && nesting > 0; k++ {
				if escaped(s, k) {
					continue
				}
				if s[k] == '(' {
					nesting++
				}
				if s[k] == ')' {
					nesting--
				}
			}
			mark(start, k)
			i = k - 1
		} else if len(defs) > 0 && defs[strings.ToLower(s[start+1:j-1])] {
			mark(start, j)
			// [label][reference] protects both labels, not just the second.
			if start > 0 && s[start-1] == ']' {
				for k := start - 2; k >= 0 && s[k] != '\n'; k-- {
					if s[k] == '[' && !escaped(s, k) {
						mark(k, j)
						break
					}
				}
			}
		}
	}
	return mask
}
