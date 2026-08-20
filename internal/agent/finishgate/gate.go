// Package finishgate decides whether an agent summary run is COMPLETE, PARTIAL,
// or FAILED (SS-07), replacing the implicit "the model stopped calling tools, so
// the answer is final" rule with an explicit, auditable verdict.
//
// The motivating failure: a run that silently dropped 60% of its messages can
// still produce a confident "everything is normal" summary. Coverage/citation
// fixes (SS-01/05/06) stop the loss, but the system must also KNOW when a result
// is incomplete and disclose it rather than pass it off as COMPLETE.
//
// Evaluate is a pure function of RunState so the policy is unit-testable and the
// same logic serves both the finalize path (SS-07) and a future runner-integrated
// bounded-retry loop (SS-07b).
package finishgate

// Verdict is the generation-quality outcome. Distinct from the task-flow status.
type Verdict string

const (
	Complete Verdict = "COMPLETE"
	Partial  Verdict = "PARTIAL"
	Failed   Verdict = "FAILED"
)

// GapKind enumerates the disclosed coverage-gap categories.
const (
	GapChannel    = "channel"    // an expected channel was not fetched
	GapCoverage   = "coverage"   // channel coverage was never measured
	GapTruncation = "truncation" // the fetched pool was truncated
	GapDropped    = "dropped"    // messages were dropped before the model
	GapCitation   = "citation"   // citation integrity did not hold
	GapToolError  = "tool_error" // a critical tool failed
)

// Gap is a structured disclosure of one coverage/quality shortfall, surfaced to
// the user on PARTIAL (and explaining a FAILED).
//
// ErrorCode was removed after ChannelID was added: every producer moved to the
// typed field and it shipped as an always-absent key in a client-facing contract.
type Gap struct {
	Kind      string `json:"kind"`
	Detail    string `json:"detail"`
	ChannelID string `json:"channel_id,omitempty"`
}

// RunState is the evidence the gate reasons over. Fields default to the safe
// "nothing known" zero value; the finalize path fills what it can observe, and
// SS-07b fills the rest (tool errors, expected-channel count from the spec).
type RunState struct {
	// ScopeResolved is true once a valid Spec exists for the run.
	ScopeResolved bool
	// HasUsableEvidence is true when at least one message was gathered.
	HasUsableEvidence bool
	// SummaryGenerated is true when a non-empty summary was produced.
	SummaryGenerated bool
	// CitationValidationPassed is true when every [n] marker in the summary
	// resolves to a real citation.
	CitationValidationPassed bool

	// FetchExpected is whether this turn was supposed to gather data at all.
	//
	// It is false for turns that are fetch-free BY DESIGN: SS-08b strips the
	// fetch tools from a confident rewrite, so `coverage_measured` physically
	// cannot be set. Without this flag the gate reported PARTIAL with a coverage
	// gap on exactly the turns whose zero-fetch was the intended behaviour — one
	// half of this PR disclosing the other half's correctness as a defect.
	//
	// "Not measured" is only a gap when measurement was expected.
	FetchExpected bool

	// Coverage facts. CoverageMeasured is explicit because an empty set may mean
	// either "we fetched a quiet channel" or "no coverage path ran at all".
	CoverageMeasured  bool
	ExpectedChannels  []string
	AttemptedChannels []string
	SucceededChannels []string
	Truncated         bool
	DroppedMessages   int
	FailedChannels    []string

	// DiscoveredChannels are the channels the run learned were in scope but that
	// no spec pinned — the open-scope case, where ExpectedChannels is empty
	// because nothing was selected in the UI. Without it an open-scope run that
	// fetched 2 of 12 available channels reported COMPLETE with no gaps, which is
	// the exact under-fetch the gate exists to catch.
	DiscoveredChannels []string

	// CriticalToolErrors are unrecoverable tool failures (permission, evidence
	// write, summary). Any entry forces FAILED.
	CriticalToolErrors []string
}

// Evaluate returns the verdict and, for PARTIAL/FAILED, the disclosed gaps.
//
// Policy (docs §2.1 缺点三 verdict table):
//   - FAILED: no usable evidence / no summary, a critical tool failure, or
//     citation integrity failed. No saveable COMPLETE product.
//   - COMPLETE: scope resolved, usable evidence, summary generated, citations
//     valid, and NO undisclosed gap (no truncation, no dropped messages, all
//     expected channels fetched).
//   - PARTIAL: usable evidence and a summary, but at least one coverage gap.
//
// Coverage is judged against what the turn was SUPPOSED to do (FetchExpected)
// and against everything it knew was in scope (ExpectedChannels for a closed
// scope, DiscoveredChannels for an open one). Judging "was coverage measured"
// alone made the gate strict on runs whose coverage was complete and silent on
// the open-scope runs that actually under-fetched — backwards in both halves.
func Evaluate(s RunState) (Verdict, []Gap) {
	// Hard failures first — nothing usable to save.
	if !s.HasUsableEvidence || !s.SummaryGenerated {
		return Failed, []Gap{{Kind: GapToolError, Detail: "no usable evidence or summary produced"}}
	}
	var gaps []Gap
	for _, e := range s.CriticalToolErrors {
		gaps = append(gaps, Gap{Kind: GapToolError, Detail: e})
	}
	if len(gaps) > 0 {
		return Failed, gaps
	}
	if !s.CitationValidationPassed {
		return Failed, []Gap{{Kind: GapCitation, Detail: "citation integrity check failed"}}
	}

	// Usable + valid: collect any coverage gaps → PARTIAL, else COMPLETE.
	switch {
	case !s.FetchExpected:
		// A turn that was never supposed to fetch has nothing to disclose. This is
		// the confident-rewrite / answer-from-history shape: SS-08b removes the
		// fetch tools on purpose, so treating the resulting absence of coverage as
		// a gap would make PARTIAL the standing verdict for every correct rewrite.
	case !s.CoverageMeasured:
		// Measurement WAS expected and did not happen — unknown, not zero. Disclose
		// it rather than asserting completeness over data we never looked at.
		gaps = append(gaps, Gap{Kind: GapCoverage, Detail: "channel coverage was not measured"})
	default:
		succeeded := stringSet(s.SucceededChannels)
		failed := stringSet(s.FailedChannels)
		attempted := stringSet(s.AttemptedChannels)
		reported := make(map[string]bool)
		for _, ch := range s.ExpectedChannels {
			if reported[ch] {
				continue
			}
			reported[ch] = true
			switch {
			case failed[ch]:
				gaps = append(gaps, Gap{Kind: GapChannel, Detail: "channel fetch failed", ChannelID: ch})
			case !succeeded[ch]:
				gaps = append(gaps, Gap{Kind: GapChannel, Detail: "expected channel was not fetched", ChannelID: ch})
			}
		}
		// Open scope: nothing was pinned by a spec, so the only way to notice an
		// under-fetch is to compare what the run DISCOVERED against what it
		// attempted. "总结我这周所有群的进展" that lists 12 channels and fetches 2
		// previously reported COMPLETE with no gaps.
		for _, ch := range s.DiscoveredChannels {
			if reported[ch] || attempted[ch] {
				continue
			}
			reported[ch] = true
			gaps = append(gaps, Gap{Kind: GapChannel, Detail: "in-scope channel was never fetched", ChannelID: ch})
		}
		for _, ch := range s.FailedChannels {
			if !reported[ch] {
				gaps = append(gaps, Gap{Kind: GapChannel, Detail: "channel fetch failed", ChannelID: ch})
				reported[ch] = true
			}
		}
	}
	if s.Truncated {
		gaps = append(gaps, Gap{Kind: GapTruncation, Detail: "fetched message pool was truncated"})
	}
	if s.DroppedMessages > 0 {
		gaps = append(gaps, Gap{Kind: GapDropped, Detail: "messages were dropped before summarization"})
	}

	if !s.ScopeResolved || len(gaps) > 0 {
		if !s.ScopeResolved {
			gaps = append(gaps, Gap{Kind: GapToolError, Detail: "scope not fully resolved"})
		}
		return Partial, gaps
	}
	return Complete, nil
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}
