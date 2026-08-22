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
	// GapOutputTruncation reports that the MODEL'S OWN OUTPUT hit its token
	// ceiling and stopped mid-sentence. Deliberately NOT folded into
	// GapTruncation: the two describe opposite halves of the pipeline and need
	// opposite remediation. GapTruncation means "we did not read everything"
	// (retry with a narrower time range, or raise the per-channel cap);
	// GapOutputTruncation means "we read everything but could not WRITE it all"
	// (retry with a lower detail level or fewer output sections). Raising the
	// fetch cap on an output-truncated run makes it strictly worse. They can
	// also co-occur, and a single boolean would report only one of them.
	GapOutputTruncation = "output_truncation"
	GapDropped          = "dropped"    // messages were dropped before the model
	GapCitation         = "citation"   // citation integrity did not hold
	GapToolError        = "tool_error" // a critical tool failed
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

	// OutputTruncated reports that a model completion on the answer path was cut
	// off by finish_reason=length, i.e. the deliverable itself is unfinished.
	//
	// Its own field rather than a second writer to Truncated: the two facts come
	// from different halves of the pipeline and carry different advice (see
	// GapOutputTruncation). Folding them together would have made the existing
	// "fetched message pool was truncated" wording wrong for every
	// output-truncated run — telling a user to narrow a time range that was never
	// the problem. It sits outside the coverage group above for the same reason:
	// it is a property of the GENERATION, not of the fetch.
	//
	// Fed from the run row (output_truncated), set by the producing paths through
	// the SAME store-and-column channel every other run fact uses
	// (RecordChannelFetch / AddDroppedMessages). That is what makes the guarantee
	// hold: the disclosure is assembled from persisted evidence OUTSIDE the
	// model's control, so a planner that rewrites or drops the inline prose
	// notice cannot suppress it.
	OutputTruncated bool

	// DiscoveredChannels are the channels the run deliberately NARROWED to when no
	// spec pinned a scope — the open-scope case, where ExpectedChannels is empty
	// because nothing was selected in the UI. It is fed by the narrowing tools
	// (narrow_channels_by_topic / find_shared_channels), NOT by list_channels: the
	// raw visible surface is not the run's scope, and recording it made a perfect
	// single-channel summary report every other visible channel as unfetched.
	// Only consulted for open scope (see Evaluate); without it an open-scope run
	// that narrowed to 12 channels and fetched 2 reported COMPLETE with no gaps —
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
	//
	// FACT BEFORE EXPECTATION. CoverageMeasured is a fact (a fetch was recorded);
	// FetchExpected is an expectation (one should have been). The fact is examined
	// first and the expectation only ever explains an ABSENCE — it can never
	// suppress a recorded outcome.
	//
	// The previous order let the expectation short-circuit the whole audit: with
	// FetchExpected=false the coverage branch was skipped even when failed_channels
	// held a real failure, so a fetch that genuinely failed reported COMPLETE. That
	// reachable case is a soft rewrite — route.Fetch=false, so FetchExpected=false,
	// but the fetch tools are deliberately KEPT (only a confident rewrite strips
	// them), so the model may fetch and that fetch may fail. Ordering on the fact
	// removes the class rather than the instance: no flag can erase what happened.
	switch {
	case s.CoverageMeasured:
		// A fetch happened. Audit it in full, whatever the turn was supposed to do.
	case s.FetchExpected:
		// Nothing recorded, and a fetch WAS expected — unknown, not zero. Disclose it
		// rather than asserting completeness over data we never looked at.
		gaps = append(gaps, Gap{Kind: GapCoverage, Detail: "channel coverage was not measured"})
	default:
		// Nothing recorded and none expected: nothing to disclose. This is the
		// confident-rewrite / answer-from-history shape — SS-08b removes the fetch
		// tools on purpose, so treating that absence as a gap would make PARTIAL the
		// standing verdict for every correct rewrite.
	}
	if s.CoverageMeasured {
		succeeded := stringSet(s.SucceededChannels)
		attempted := stringSet(s.AttemptedChannels)
		reported := make(map[string]bool)

		// RECORDED OUTCOMES — audited unconditionally. A fetch that was tried and
		// failed is a fact; no expectation may suppress it. This half is what the
		// fact-before-expectation ordering above exists for.
		for _, ch := range s.FailedChannels {
			if reported[ch] {
				continue
			}
			reported[ch] = true
			gaps = append(gaps, Gap{Kind: GapChannel, Detail: "channel fetch failed", ChannelID: ch})
		}

		// ABSENCE — "in scope but never fetched" is the *absence* of a fetch, so it
		// stays explainable by the expectation, exactly like the unmeasured case above.
		//
		// Applying fact-before-expectation to this half too was an over-correction:
		// a soft rewrite KEEPS its fetch tools, so a turn with three channels still
		// pinned in the UI whose model opportunistically touched one to check a
		// detail became PARTIAL with two bogus "was not fetched" gaps shipped to the
		// client — strictly worse than the COMPLETE it reported before. The turn was
		// never obliged to cover that scope; one voluntary fetch does not create the
		// obligation.
		if s.FetchExpected {
			for _, ch := range s.ExpectedChannels {
				if reported[ch] {
					continue
				}
				reported[ch] = true
				if !succeeded[ch] {
					gaps = append(gaps, Gap{Kind: GapChannel, Detail: "expected channel was not fetched", ChannelID: ch})
				}
			}
			// Open scope ONLY. When a spec pinned channels, ExpectedChannels above is
			// authoritative and the discovered union must not manufacture gaps for
			// channels the run deliberately left out of a closed scope — a user who
			// pins one channel and gets a perfect single-channel summary was being told
			// every other discovered channel "was never fetched". For an open scope
			// (nothing pinned) the discovered-vs-attempted delta is the only under-fetch
			// signal: "总结我这周所有群的进展" that narrowed to 12 channels and fetched 2
			// previously reported COMPLETE with no gaps.
			if len(s.ExpectedChannels) == 0 {
				for _, ch := range s.DiscoveredChannels {
					if reported[ch] || attempted[ch] {
						continue
					}
					reported[ch] = true
					gaps = append(gaps, Gap{Kind: GapChannel, Detail: "in-scope channel was never fetched", ChannelID: ch})
				}
			}
		}
	}
	if s.Truncated {
		gaps = append(gaps, Gap{Kind: GapTruncation, Detail: "fetched message pool was truncated"})
	}
	// Independent of the pool check above, and unconditional on CoverageMeasured:
	// an output truncation is a property of the GENERATION, not of the fetch, so
	// it must be disclosed even on turns that legitimately fetched nothing (a
	// confident rewrite can still overrun its token ceiling).
	if s.OutputTruncated {
		gaps = append(gaps, Gap{Kind: GapOutputTruncation, Detail: "model output was truncated at its token limit; the summary is unfinished"})
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
