package db

import (
	"strings"
	"testing"

	migrationsql "github.com/Mininglamp-OSS/octo-smart-summary/migrations/sql"
)

func TestSummaryWorkflowIdempotencyMigrationUsesBinaryKeyCollation(t *testing.T) {
	raw, err := migrationsql.FS.ReadFile("20260826-01-summary-workflow-idempotency.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if !strings.Contains(
		string(raw),
		"idempotency_key VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL",
	) {
		t.Fatal("workflow idempotency key must use byte-exact utf8mb4_0900_bin collation")
	}
}
