package service

import "fmt"

// Mixed document+chat source invariants (formerly also held the phase-1
// admission gate). The admission gate was removed when the worker executor
// landed: mixed document+chat creation is now admitted unconditionally, and
// what remains here are the shared mixed-scope bounds shared by the service
// boundary and the workspace normalize path.

// MixedMaxTotalSources bounds the COMBINED document+chat source count of a
// mixed scope. Documents additionally respect MaxDocumentSummarySourceCount.
const MixedMaxTotalSources = 30

// MixedSourceCountExceededError returns the standard rejection for a mixed
// scope over MixedMaxTotalSources.
func MixedSourceCountExceededError() *BizError {
	return NewBizError(40001, fmt.Sprintf("混合来源总数不能超过%d个", MixedMaxTotalSources), 400)
}
