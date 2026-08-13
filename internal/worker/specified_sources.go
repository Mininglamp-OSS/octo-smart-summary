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
func explicitSpecifiedSources(sources []model.SummarySource) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(sources))
	for _, s := range sources {
		out = append(out, map[string]interface{}{
			"source_id":   s.SourceID,
			"source_type": s.SourceType,
			"source_name": s.SourceName,
		})
	}
	return out
}
