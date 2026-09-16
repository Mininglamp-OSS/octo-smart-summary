package service

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citationtext"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

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

// NormalizeGeneratedCitations is shared by Agent saves and refinement. Match the
// generation path's prose-safe policy: only adjacent citation clusters are
// expanded, while isolated bracketed ranges remain byte-identical prose. Single
// markers inside the known evidence window must still resolve.
func NormalizeGeneratedCitations(content string, citations []model.Citation) (string, error) {
	indices := make(map[int]bool, len(citations))
	maxIndex := 0
	for _, c := range citations {
		indices[c.Index] = true
		if c.Index > maxIndex {
			maxIndex = c.Index
		}
	}
	normalized := citationtext.CanonicalizeAdjacent(content, func(n int) bool { return indices[n] })
	for _, marker := range citationtext.Scan(normalized) {
		if marker.Compound || len(marker.Indices) != 1 {
			continue
		}
		n := marker.Indices[0]
		if n <= maxIndex && !indices[n] {
			return content, citationtext.ErrInvalid
		}
	}
	return normalized, nil
}

func extractReferencedIndices(content string) map[int]bool {
	result := make(map[int]bool)
	for _, marker := range citationtext.Scan(content) {
		for _, n := range marker.Indices {
			result[n] = true
		}
	}
	return result
}
