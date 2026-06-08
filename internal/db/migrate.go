package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	migrate "github.com/rubenv/sql-migrate"

	migrationsql "github.com/Mininglamp-OSS/octo-smart-summary/migrations/sql"
)

const migrationLockName = "smart_summary_migration"
const migrationLockTimeout = 30
const migrationTableName = "gorp_migrations"

// stalePR62MigrationIDs are migration files that existed only in intermediate
// commits of PR #62 and were dropped by the clean re-integration (PR #77).
// Any environment that deployed a PR #62 commit recorded these IDs in
// gorp_migrations. sql-migrate (v1.7.1) fails its planning phase with
// "unknown migration in database" when the recorded set is not a subset of the
// embedded files -- before any migration (including a cleanup migration) runs,
// so the cleanup must happen here in Go, ahead of migrate.Exec.
//
// This list is an explicit allow-list: it never contains IDs shipped on main or
// the two IDs PR #77 itself ships, so a healthy DB is left untouched.
var stalePR62MigrationIDs = []string{
	"20260604-01-add-interval-days.sql",
	"20260604-02-add-interval-months-runtime.sql",
	"20260604-03-clean-dirty-schedules-backfill-runtime.sql",
	"20260604-04-add-dow-dom.sql",
	"20260608-01-unique-live-schedule-binding.sql",
	"20260608-02-clean-dirty-participant-config.sql",
	"20260608-03-add-anchor-dom.sql",
	"20260608-04-backfill-anchor-dom.sql",
	"20260608-05-clean-duplicate-summary-participant.sql",
	"20260608-06-add-unique-summary-participant-task-user.sql",
	"20260608-07-heal-partial-ddl-state.sql",
	"20260609-01-task-schedule-binding.sql",
	"20260609-02-schedule-anchor-and-config.sql",
	"20260609-03-participant-dedup-unique.sql",
}

func RunMigrations(db *sql.DB) (int, error) {
	if os.Getenv("SKIP_MIGRATION") == "true" {
		log.Printf("[migrate] SKIP_MIGRATION=true, skipping")
		return 0, nil
	}

	ctx := context.Background()

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get db connection for migration lock: %w", err)
	}
	defer conn.Close()

	var lockResult int
	err = conn.QueryRowContext(ctx,
		"SELECT GET_LOCK(?, ?)", migrationLockName, migrationLockTimeout,
	).Scan(&lockResult)
	if err != nil {
		return 0, fmt.Errorf("migration lock query failed: %w", err)
	}
	if lockResult != 1 {
		return 0, fmt.Errorf("failed to acquire migration lock (timeout %ds)", migrationLockTimeout)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", migrationLockName) }()

	// Remove orphaned PR #62 migration records before planning (see
	// stalePR62MigrationIDs). Runs inside the migration lock; safe no-op on a
	// clean DB.
	if err := purgeStaleMigrationRecords(ctx, conn); err != nil {
		return 0, fmt.Errorf("purge stale migration records: %w", err)
	}

	source := &migrate.EmbedFileSystemMigrationSource{
		FileSystem: migrationsql.FS,
		Root:       ".",
	}
	return runMigrationsCore(db, "mysql", source)
}

// purgeStaleMigrationRecords deletes the known-orphan PR #62 migration ids from
// gorp_migrations so sql-migrate's unknown-migration planning check passes on
// environments that previously deployed PR #62. It is idempotent and only
// touches the explicit allow-list; if the migrations table does not exist yet
// (fresh DB) it is a no-op.
func purgeStaleMigrationRecords(ctx context.Context, conn *sql.Conn) error {
	var tableCount int
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
		migrationTableName,
	).Scan(&tableCount); err != nil {
		return err
	}
	if tableCount == 0 {
		return nil // fresh DB, nothing recorded yet
	}

	placeholders := make([]string, len(stalePR62MigrationIDs))
	args := make([]interface{}, len(stalePR62MigrationIDs))
	for i, id := range stalePR62MigrationIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf("DELETE FROM `%s` WHERE id IN (%s)", migrationTableName, joinComma(placeholders))
	res, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[migrate] purged %d orphaned PR#62 migration record(s) before planning", n)
	}
	return nil
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func runMigrationsCore(db *sql.DB, dialect string, source migrate.MigrationSource) (int, error) {
	n, err := migrate.Exec(db, dialect, source, migrate.Up)
	if err != nil {
		return 0, fmt.Errorf("migration failed: %w", err)
	}

	return n, nil
}
