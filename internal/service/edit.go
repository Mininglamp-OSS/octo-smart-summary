package service

import (
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citation"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

var citationRefRe = citation.MarkerRe

func CleanUnreferencedCitations(content string, citations []model.Citation) []model.Citation {
	referenced := extractReferencedIndices(content)
	var kept []model.Citation
	for _, c := range citations {
		if referenced[c.Index] {
			kept = append(kept, c)
		}
	}
	return kept
}

func extractReferencedIndices(content string) map[int]bool {
	result := make(map[int]bool)
	fencedRanges := findFencedCodeRanges(content)
	inlineRanges := findInlineCodeRanges(content)

	matches := citationRefRe.FindAllStringSubmatchIndex(content, -1)
	for _, m := range matches {
		matchStart := m[0]
		matchEnd := m[1]

		if inRange(matchStart, fencedRanges) || inRange(matchStart, inlineRanges) {
			continue
		}

		if matchEnd < len(content) && content[matchEnd] == '(' {
			continue
		}

		numStr := content[m[2]:m[3]]
		idx, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		result[idx] = true
	}
	return result
}

type textRange struct {
	start, end int
}

func findFencedCodeRanges(content string) []textRange {
	var ranges []textRange
	lines := strings.Split(content, "\n")
	pos := 0
	inFence := false
	fenceStart := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				inFence = true
				fenceStart = pos
			} else {
				ranges = append(ranges, textRange{fenceStart, pos + len(line)})
				inFence = false
			}
		}
		pos += len(line) + 1
	}
	if inFence {
		ranges = append(ranges, textRange{fenceStart, len(content)})
	}
	return ranges
}

func findInlineCodeRanges(content string) []textRange {
	var ranges []textRange
	i := 0
	for i < len(content) {
		if content[i] == '`' {
			if i+2 < len(content) && content[i:i+3] == "```" {
				i++
				continue
			}
			end := strings.Index(content[i+1:], "`")
			if end == -1 {
				break
			}
			ranges = append(ranges, textRange{i, i + 1 + end + 1})
			i = i + 1 + end + 1
		} else {
			i++
		}
	}
	return ranges
}

func inRange(pos int, ranges []textRange) bool {
	for _, r := range ranges {
		if pos >= r.start && pos < r.end {
			return true
		}
	}
	return false
}
