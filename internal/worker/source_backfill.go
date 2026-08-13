package worker

import (
	"fmt"
	"log"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm/clause"
)

// backfillMaxSources mirrors maxSourceCount on the create endpoint
// (api/handler/task.go). The handler package cannot be imported here
// (import cycle), so the cap is duplicated intentionally — keep in sync.
const backfillMaxSources = 30

// backfillSourcesFromMessages persists the channels a task actually fetched
// messages from as summary_source rows, so tasks created WITHOUT explicit
// sources (personal / multi-person summaries where the pipeline auto-selects
// channels) still carry a source footprint.
//
// Why this exists: reference → refine → save's tier-4 origin derivation
// (agent_summary.go deriveOriginFromSummarySources) reads summary_source to
// inherit an origin channel. A zero-row task can never inherit and fails
// with 40001 — reproduced by task 141 (ST20260813kbfwc36r, BY_PERSON,
// 2 participants, 0 source rows) while task 128 (same mode, 2 source rows)
// worked. The auto-select path kept the chosen channels only in memory
// (channelSet stats) and never wrote them back; this closes that gap.
//
// Semantics:
//   - append-only: existing rows (user-specified at creation) are never
//     modified or deleted; only (source_type, source_id) pairs not already
//     present are added
//   - idempotent: safe to re-run on task retries (executePipeline re-runs)
//   - messages with an unmappable storage channel_type (WuKongIM reserved
//     values 3/4, unset 0) or empty channel_id are skipped — never persist
//     a garbage source_type
//   - R9 P1 (PR #190): DM channels are NEVER backfilled. A DM's source_id
//     names the peer UID, and persisting it would expose the creator's
//     private-chat partners to any task participant (roster is mutable after
//     generation); a DM is also never a useful origin_channel_id for a
//     refined summary, and a normalized uidA@uidB id (2×32-char UIDs = 65
//     chars) overflows summary_source.source_id varchar(64)
//   - every persisted row carries Derived=true so user-visible projections
//     (list/detail sources, share snapshot, reference-context bullets) can
//     exclude it while tier-4 origin derivation keeps reading it
//   - total rows capped at backfillMaxSources; existing rows count first
//
// Returns an error only on real DB failures; callers log and continue —
// this is best-effort enrichment and must never block summary generation.
func (p *Processor) backfillSourcesFromMessages(taskID int64, messages []pipeline.Message) error {
	if len(messages) == 0 {
		return nil
	}

	var existing []model.SummarySource
	if err := p.db.Where("task_id = ?", taskID).Find(&existing).Error; err != nil {
		return fmt.Errorf("load existing sources: %w", err)
	}
	seen := make(map[string]struct{}, len(existing))
	for _, s := range existing {
		seen[sourceBackfillKey(s.SourceType, s.SourceID)] = struct{}{}
	}

	additions := make([]model.SummarySource, 0)
	for _, m := range messages {
		if m.ChannelID == "" {
			continue
		}
		sourceType, ok := pipeline.StorageChannelTypeToSourceType(m.ChannelType)
		if !ok {
			continue
		}
		// R9 P1: DM channels are never backfilled (see doc comment).
		if sourceType == model.SourceDirect {
			continue
		}
		key := sourceBackfillKey(sourceType, m.ChannelID)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		name := m.SourceName
		if name == "" {
			// Both fetch backends normally fill SourceName from the channel
			// info; fall back to IM-db resolution (nil imDB degrades to a
			// placeholder) so rows never ship with an empty display name.
			name = service.ResolveSourceNameWithType(m.ChannelID, sourceType, p.imDB)
		}
		additions = append(additions, model.SummarySource{
			TaskID:     taskID,
			SourceType: sourceType,
			SourceID:   m.ChannelID,
			SourceName: name,
			Derived:    true,
		})
	}

	// Cap: existing rows win; new rows fill up to backfillMaxSources.
	room := backfillMaxSources - len(existing)
	if room <= 0 || len(additions) == 0 {
		return nil
	}
	if len(additions) > room {
		additions = additions[:room]
	}

	// R10 (yujiawei, issue comment 5280351017): conflict-tolerant insert.
	// The read-then-insert dedup above holds only for serial retries;
	// scanStuckTasks can re-dispatch the task on lease expiry while the
	// original worker is still running, so two backfills can race with
	// empty reads. uk_summary_source_task_type_id (migration
	// 20260814-01) + INSERT IGNORE makes idempotency structural: the loser
	// silently skips rows the winner already persisted.
	if err := p.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "task_id"}, {Name: "source_type"}, {Name: "source_id"}},
		DoNothing: true,
	}).Create(&additions).Error; err != nil {
		return fmt.Errorf("insert %d backfilled sources: %w", len(additions), err)
	}
	log.Printf("[processor] task %d: backfilled %d summary_source rows from fetched channels", taskID, len(additions))
	return nil
}

func sourceBackfillKey(sourceType int, sourceID string) string {
	return fmt.Sprintf("%d|%s", sourceType, sourceID)
}
