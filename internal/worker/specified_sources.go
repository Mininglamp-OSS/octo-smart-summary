package worker

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// explicitSpecifiedSources converts a task's persisted summary_source rows
// into the pipeline's specifiedSources shape (source_id / source_type /
// source_name maps consumed by ApplySourceConstraints).
//
// Extracted from the identical inline loops in executePipeline (processor.go)
// and executePersonalPipeline (personal_processor.go) so the fetch-input
// contract is testable in isolation.
//
// R10 (Jerry-Xin, review 4928758044): derived rows (worker backfill of
// auto-selected channels) are EXCLUDED. They are a tier-4 origin-derivation
// footprint, not user-specified constraints — source_backfill.go writes them
// before personal generation dispatch, so feeding them back here would turn
// auto-selected channels from the previous run into hard constraints on the
// next (changing channel selection behaviour in a way nobody intended).
func explicitSpecifiedSources(sources []model.SummarySource) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(sources))
	for _, s := range sources {
		if s.Derived {
			continue
		}
		out = append(out, map[string]interface{}{
			"source_id":   s.SourceID,
			"source_type": s.SourceType,
			"source_name": s.SourceName,
		})
	}
	return out
}
