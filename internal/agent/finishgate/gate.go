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
	GapTruncation = "truncation" // the fetched pool was truncated
	GapDropped    = "dropped"    // messages were dropped before the model
	GapCitation   = "citation"   // citation integrity did not hold
	GapToolError  = "tool_error" // a critical tool failed
)

// Gap is a structured disclosure of one coverage/quality shortfall, surfaced to
// the user on PARTIAL (and explaining a FAILED).
type Gap struct {
	Kind      string `json:"kind"`
	Detail    string `json:"detail"`
	ErrorCode string `json:"error_code,omitempty"`
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

	// Coverage facts (0 / false = unknown / none).
	ChannelsExpected int
	ChannelsFetched  int
	Truncated        bool
	DroppedMessages  int
	FailedChannels   []string

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
	if s.ChannelsExpected > 0 && s.ChannelsFetched < s.ChannelsExpected {
		gaps = append(gaps, Gap{
			Kind:   GapChannel,
			Detail: "not all expected channels were fetched",
		})
	}
	for _, ch := range s.FailedChannels {
		gaps = append(gaps, Gap{Kind: GapChannel, Detail: "channel fetch failed", ErrorCode: ch})
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
