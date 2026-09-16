package citationtext

// ValidAdjacent is the prose-safe counterpart of Valid for generation paths
// whose evidence window is a contiguous 1..N range (Agent draft/emit). It keeps
// the two hard guarantees of the strict boundary (review 5087740714 blocker 4):
// every single marker and every cluster-resolvable compound group must resolve
// inside the evidence window, and requireMarker still demands at least one real
// citation marker. What it adds is the CanonicalizeAdjacent adjacency rule: an
// isolated compound group — one not abutting another citation marker — is
// ordinary bracketed-number prose (dates, standards, page ranges) and is
// neither rejected nor counted as evidence. Malformed or unauthorized groups
// inside a real citation cluster still fail closed.
func ValidAdjacent(content string, valid func(int) bool, requireMarker bool) bool {
	markers := Scan(content)
	hasCitation := false
	for i, m := range markers {
		if !m.Compound {
			// Single markers keep the strict contract: they are citations.
			if len(m.Indices) == 0 {
				return false
			}
			for _, n := range m.Indices {
				if valid == nil || !valid(n) {
					return false
				}
			}
			hasCitation = true
			continue
		}
		if !adjacentToMarker(content, markers, i) {
			// Isolated compound groups are prose, not evidence — even when
			// malformed, so stray shapes never fail an otherwise valid draft.
			continue
		}
		if len(m.Indices) == 0 {
			// Malformed groups abutting a real cluster are corruption, not
			// prose: the writer emitted citation syntax and damaged it.
			return false
		}
		// A group inside a real citation cluster must resolve in full, so a
		// damaged or corrupt member can never hide behind its neighbours.
		for _, n := range m.Indices {
			if valid == nil || !valid(n) {
				return false
			}
		}
		hasCitation = true
	}
	return !requireMarker || hasCitation
}
