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

	source := &migrate.EmbedFileSystemMigrationSource{
		FileSystem: migrationsql.FS,
		Root:       ".",
	}
	return runMigrationsCore(db, "mysql", source)
}

func runMigrationsCore(db *sql.DB, dialect string, source migrate.MigrationSource) (int, error) {
	n, err := migrate.Exec(db, dialect, source, migrate.Up)
	if err != nil {
		return 0, fmt.Errorf("migration failed: %w", err)
	}

	return n, nil
}
