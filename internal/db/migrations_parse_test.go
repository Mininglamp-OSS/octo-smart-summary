package db

import (
	"testing"

	migrationsql "github.com/Mininglamp-OSS/octo-smart-summary/migrations/sql"
	migrate "github.com/rubenv/sql-migrate"
)

func TestRealMigrationsParse(t *testing.T) {
	source := &migrate.EmbedFileSystemMigrationSource{
		FileSystem: migrationsql.FS,
		Root:       ".",
	}
	if _, err := source.FindMigrations(); err != nil {
		t.Fatalf("real migration set does not parse: %v", err)
	}
}
