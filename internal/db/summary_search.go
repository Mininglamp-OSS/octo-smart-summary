package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// BackfillSummarySearch is restartable and bounded to 500 rows per transaction.
// Row locks prevent a concurrent new writer's title edit being overwritten by
// normalization of an older value. It never changes task timestamps or content.
// Run after the schema migration and before admitting API/worker traffic.
func BackfillSummarySearch(ctx context.Context, db *sql.DB) error {
	var after int64
	for {
		last, count, err := backfillSummarySearchBatch(ctx, db, after)
		if err != nil {
			return fmt.Errorf("backfill summary search: %w", err)
		}
		if count == 0 {
			return nil
		}
		after = last
	}
}

func backfillSummarySearchBatch(ctx context.Context, db *sql.DB, after int64) (int64, int, error) {
	// Find missing IDs without locking already-backfilled rows. Rechecking the
	// NULL sentinel under point-lookup locks avoids overwriting a concurrent writer.
	candidates, err := db.QueryContext(ctx, "SELECT id FROM summary_task WHERE id > ? AND (search_title IS NULL OR search_topic IS NULL) ORDER BY id LIMIT 500", after)
	if err != nil {
		return after, 0, err
	}
	ids := make([]interface{}, 0, 500)
	for candidates.Next() {
		var id int64
		if err := candidates.Scan(&id); err != nil {
			candidates.Close()
			return after, 0, err
		}
		ids = append(ids, id)
		after = id
	}
	err = candidates.Err()
	candidates.Close()
	if err != nil {
		return after, 0, err
	}
	if len(ids) == 0 {
		return after, 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return after, 0, err
	}
	defer tx.Rollback()
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := tx.QueryContext(ctx, "SELECT id, title, topic FROM summary_task WHERE id IN ("+placeholders+") AND (search_title IS NULL OR search_topic IS NULL) FOR UPDATE", ids...)
	if err != nil {
		return after, 0, err
	}
	tasks := make([]model.SummaryTask, 0, 500)
	for rows.Next() {
		var task model.SummaryTask
		if err := rows.Scan(&task.ID, &task.Title, &task.Topic); err != nil {
			rows.Close()
			return after, 0, err
		}
		tasks = append(tasks, task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return after, 0, err
	}
	for _, task := range tasks {
		// MySQL's ON UPDATE CURRENT_TIMESTAMP fires for raw SQL too. An
		// explicit self-assignment preserves the task's business edit time.
		_, err = tx.ExecContext(ctx, "UPDATE summary_task SET search_title = ?, search_topic = ?, updated_at = updated_at WHERE id = ?", model.NormalizeSummarySearch(task.Title), model.NormalizeSummarySearchTopic(task.Topic), task.ID)
		if err != nil {
			return after, 0, err
		}
	}
	return after, len(ids), tx.Commit()
}
