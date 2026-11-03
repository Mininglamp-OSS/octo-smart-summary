package handler

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

const maxSummarySearchRunes = 100

// SummarySearchMatch uses half-open Unicode code-point offsets into the original
// displayed fields, never byte/UTF-16 offsets or offsets into normalized text.
type SummarySearchMatch struct {
	TitleRanges [][2]int `json:"title_ranges"`
	TopicRanges [][2]int `json:"topic_ranges"`
}

func normalizeSummarySearch(s string) string {
	return model.NormalizeSummarySearch(s)
}

func summarySearchRanges(text, normalizedKeyword string) [][2]int {
	ranges := make([][2]int, 0)
	if normalizedKeyword == "" {
		return ranges
	}
	// Case folding may expand a character (e.g. ß -> ss). Keep a source
	// code-point index for every normalized character so highlights stay on
	// the displayed original, including full-width letters and supplementary runes.
	normalized := make([]rune, 0, len(text))
	origins := make([]int, 0, len(text))
	for i, r := range []rune(text) {
		for _, folded := range normalizeSummarySearch(string(r)) {
			normalized = append(normalized, folded)
			origins = append(origins, i)
		}
	}
	needle := []rune(normalizedKeyword)
	for i := 0; i+len(needle) <= len(normalized); i++ {
		if string(normalized[i:i+len(needle)]) != normalizedKeyword {
			continue
		}
		r := [2]int{origins[i], origins[i+len(needle)-1] + 1}
		if n := len(ranges); n > 0 && ranges[n-1][1] >= r[0] {
			if r[1] > ranges[n-1][1] {
				ranges[n-1][1] = r[1]
			}
		} else {
			ranges = append(ranges, r)
		}
	}
	return ranges
}

func matchSummarySearch(task model.SummaryTask, normalizedKeyword string) SummarySearchMatch {
	return SummarySearchMatch{
		TitleRanges: summarySearchRanges(task.Title, normalizedKeyword),
		TopicRanges: summarySearchRanges(task.DisplayTopic(), normalizedKeyword),
	}
}

// Persisted normalized fields keep candidate text and matching IDs in the DB.
// Binary comparison avoids collation expansions and treats %, _ and \ literally.
func summarySearchFilter(db *gorm.DB, where string, args []interface{}, keyword string) (string, []interface{}) {
	predicate := "(instr(search_title, ?) > 0 OR instr(search_topic, ?) > 0)"
	if db.Dialector.Name() == "mysql" {
		predicate = "(LOCATE(CAST(? AS BINARY), CAST(search_title AS BINARY)) > 0 OR LOCATE(CAST(? AS BINARY), CAST(search_topic AS BINARY)) > 0)"
	}
	return where + " AND " + predicate, append(args, keyword, keyword)
}

func normalizedSummaryKeyword(raw string) string {
	return normalizeSummarySearch(strings.TrimSpace(raw))
}
