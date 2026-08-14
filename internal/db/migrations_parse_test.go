package db

import (
	"strings"
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

func TestSummarySourceUniqueMigrationPinsBinaryCollationBeforeDedup(t *testing.T) {
	raw, err := migrationsql.FS.ReadFile("20260814-01-summary-source-unique-key.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)
	modify := strings.Index(sql, "MODIFY COLUMN source_id varchar(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin")
	dedup := strings.Index(sql, "DELETE s1 FROM summary_source")
	index := strings.Index(sql, "ADD UNIQUE INDEX uk_summary_source_task_type_id")
	if modify < 0 || dedup < 0 || index < 0 {
		t.Fatalf("migration is missing binary collation, dedup, or unique-index step")
	}
	if !(modify < dedup && dedup < index) {
		t.Fatalf("migration order must be binary collation -> dedup -> unique index")
	}
	if !strings.Contains(sql, "s1.source_id COLLATE utf8mb4_0900_bin = s2.source_id COLLATE utf8mb4_0900_bin") {
		t.Fatalf("dedup join must use byte-exact, NO PAD utf8mb4_0900_bin comparison")
	}
}
