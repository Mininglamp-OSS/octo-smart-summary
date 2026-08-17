package agent

import (
	"context"
	"fmt"
	"sort"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/artifact"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// applyFrozenManifest overrides each message's CitationIndex with the ordinal
// frozen in the run's citation manifest (SS-05 B1), closing the citation-drift
// defect: the mid-run pass and the save-time pass both read the SAME frozen
// ordinals instead of independently re-deriving them from a possibly-changed
// evidence set.
//
// Freeze-once semantics: the first call for a run freezes the manifest from the
// current pool; every later call (even if the evidence set has since grown)
// reads that first manifest. Messages fetched AFTER the freeze are not in the
// manifest and are dropped from the citable pool — matching the documented
// "no new evidence after the citation pass is frozen" invariant.
//
// Callers MUST gate on SummaryV2Enabled() && runID != "" so the off path never
// reaches here and stays byte-identical to the pre-SS-05 recompute.
//
// On any failure (no DB, freeze error) it returns the input pool unchanged, so
// the worst case is a silent fall-back to the legacy recompute — never a lost
// answer.
func applyFrozenManifest(ctx context.Context, uid, sessionID, runID string, pool []pipeline.Message) []pipeline.Message {
	db, _, _, _ := GetSummaryDeps()
	if db == nil || runID == "" {
		return pool
	}
	store := artifact.NewStore(db)

	_, entries, found, err := store.GetLatestManifestByRun(ctx, uid, runID)
	if err != nil {
		return pool
	}
	if !found {
		// First citation pass for this run: freeze the current pool.
		if _, _, _, ferr := store.FreezeFromPool(ctx, runID, uid, sessionID, pool, artifact.FreezeMeta{}); ferr != nil {
			return pool
		}
		_, entries, found, err = store.GetLatestManifestByRun(ctx, uid, runID)
		if err != nil || !found {
			return pool
		}
	}

	ord := artifact.OrdinalMap(entries)
	out := make([]pipeline.Message, 0, len(pool))
	for _, m := range pool {
		key := fmt.Sprintf("%s:%d", m.ChannelID, m.MessageSeq)
		idx, ok := ord[key]
		if !ok {
			// Post-freeze message: not citable under this manifest. Drop it.
			continue
		}
		m.CitationIndex = idx
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CitationIndex < out[j].CitationIndex })
	return out
}
