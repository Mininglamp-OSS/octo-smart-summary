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

// NormalizeGeneratedCitations is shared by Agent saves, Workflow and refinement.
// Only already-authorized citations may back a group; no source is fabricated.
func NormalizeGeneratedCitations(content string, citations []model.Citation) (string, error) {
	indices := make(map[int]bool, len(citations))
	for _, c := range citations {
		indices[c.Index] = true
	}
	return citationtext.Canonicalize(content, func(n int) bool { return indices[n] })
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
