// Package eval is the SS-02 minimal golden-set replay harness for agent
// summaries. It computes deterministic quality metrics — coverage funnel,
// citation existence, and format adherence — that run WITHOUT any external LLM,
// so they can gate CI and give the P0 stop-the-bleeding work (SS-01) a
// reproducible "before/after" baseline.
//
// Scope discipline (see docs/AGENT-SUMMARY-...zh.md §4.1 Stage 1): this harness
// intentionally does NOT score semantic faithfulness or call a model. Those
// (LLM-as-judge, NLI) belong to later stages and must be human-calibrated first.
// What lives here is only what can be checked deterministically from a fixed
// snapshot.
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
)

// GoldenMessage is one message in a fixed snapshot.
type GoldenMessage struct {
	ChannelID  string `json:"channel_id"`
	MessageSeq int64  `json:"message_seq"`
	SenderName string `json:"sender_name"`
	Content    string `json:"content"`
}

// GoldenExpectations are the fixed, human-authored expectations for a case.
type GoldenExpectations struct {
	MessageCount     int      `json:"message_count"`
	Channels         []string `json:"channels"`
	RequiredSections []string `json:"required_sections"`
	Language         string   `json:"language"`
}

// GoldenCase is one replayable fixture: a snapshot + expectations + a fixed
// exemplar summary. In offline mode the exemplar stands in for live model
// output so the citation/format checks are reproducible; a live run swaps
// SampleSummary for the model's actual answer and reuses the same metrics.
type GoldenCase struct {
	Name          string             `json:"name"`
	Description   string             `json:"description"`
	UserRequest   string             `json:"user_request"`
	Messages      []GoldenMessage    `json:"messages"`
	Expected      GoldenExpectations `json:"expected"`
	SampleSummary string             `json:"sample_summary"`
}

// LoadGoldenCases reads every *.json case under dir.
func LoadGoldenCases(dir string) ([]GoldenCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read golden dir: %w", err)
	}
	var cases []GoldenCase
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var gc GoldenCase
		if err := json.Unmarshal(raw, &gc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		cases = append(cases, gc)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })
	return cases, nil
}

// CoverageMetric reports the data-coverage funnel for a case: how many snapshot
// messages the current chunking path actually feeds to the model. Computed via
// agent.ComputeCoverage so it validates production logic, not a copy.
type CoverageMetric struct {
	InputCount     int  `json:"input_count"`
	ProcessedCount int  `json:"processed_count"`
	DroppedCount   int  `json:"dropped_count"`
	Chunks         int  `json:"chunks"`
	NoSilentLoss   bool `json:"no_silent_loss"`
}

// Coverage runs the funnel for a case at the default chunk_size (0 → default).
func Coverage(gc GoldenCase) CoverageMetric {
	processed, dropped, chunks := agent.ComputeCoverage(len(gc.Messages), 0)
	return CoverageMetric{
		InputCount:     len(gc.Messages),
		ProcessedCount: processed,
		DroppedCount:   dropped,
		Chunks:         chunks,
		NoSilentLoss:   dropped == 0,
	}
}

var citationMarker = regexp.MustCompile(`\[(\d+)\]`)

// CitationMetric reports whether every [n] marker in a summary resolves to a
// real message ordinal in [1, MaxIndex]. Mirrors worker.BuildCitations's
// accept rule (idx >= 1 && idx <= maxIdx) so an out-of-range marker is caught
// offline instead of being silently dropped at save time.
type CitationMetric struct {
	MaxIndex      int   `json:"max_index"`
	Total         int   `json:"total"`
	Valid         int   `json:"valid"`
	OutOfRange    []int `json:"out_of_range"`
	AllResolvable bool  `json:"all_resolvable"`
}

// Citations validates the markers in summary against a pool of maxIndex messages.
func Citations(summary string, maxIndex int) CitationMetric {
	m := CitationMetric{MaxIndex: maxIndex}
	for _, match := range citationMarker.FindAllStringSubmatch(summary, -1) {
		var n int
		if _, err := fmt.Sscanf(match[1], "%d", &n); err != nil {
			continue
		}
		m.Total++
		if n >= 1 && n <= maxIndex {
			m.Valid++
		} else {
			m.OutOfRange = append(m.OutOfRange, n)
		}
	}
	m.AllResolvable = m.Total > 0 && m.Valid == m.Total
	return m
}

// FormatMetric reports whether a summary satisfies the case's structural and
// language expectations.
type FormatMetric struct {
	MissingSections []string `json:"missing_sections"`
	LanguageOK      bool     `json:"language_ok"`
	Adherent        bool     `json:"adherent"`
}

var hanChar = regexp.MustCompile(`\p{Han}`)

// Format checks required sections are present and the language heuristic holds.
func Format(gc GoldenCase, summary string) FormatMetric {
	var missing []string
	for _, sec := range gc.Expected.RequiredSections {
		if !strings.Contains(summary, sec) {
			missing = append(missing, sec)
		}
	}
	langOK := true
	if gc.Expected.Language == "zh" {
		langOK = hanChar.MatchString(summary)
	}
	return FormatMetric{
		MissingSections: missing,
		LanguageOK:      langOK,
		Adherent:        len(missing) == 0 && langOK,
	}
}

// CaseReport bundles all three metrics for one case.
type CaseReport struct {
	Name     string         `json:"name"`
	Coverage CoverageMetric `json:"coverage"`
	Citation CitationMetric `json:"citation"`
	Format   FormatMetric   `json:"format"`
}

// Evaluate runs all deterministic metrics for a case using its exemplar summary.
func Evaluate(gc GoldenCase) CaseReport {
	return CaseReport{
		Name:     gc.Name,
		Coverage: Coverage(gc),
		Citation: Citations(gc.SampleSummary, len(gc.Messages)),
		Format:   Format(gc, gc.SampleSummary),
	}
}

// String renders a compact one-block report (used by the CLI / test -v output).
func (r CaseReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "case=%s\n", r.Name)
	fmt.Fprintf(&b, "  coverage: input=%d processed=%d dropped=%d chunks=%d no_silent_loss=%t\n",
		r.Coverage.InputCount, r.Coverage.ProcessedCount, r.Coverage.DroppedCount, r.Coverage.Chunks, r.Coverage.NoSilentLoss)
	fmt.Fprintf(&b, "  citation: total=%d valid=%d out_of_range=%v all_resolvable=%t\n",
		r.Citation.Total, r.Citation.Valid, r.Citation.OutOfRange, r.Citation.AllResolvable)
	fmt.Fprintf(&b, "  format:   missing_sections=%v language_ok=%t adherent=%t",
		r.Format.MissingSections, r.Format.LanguageOK, r.Format.Adherent)
	return b.String()
}
