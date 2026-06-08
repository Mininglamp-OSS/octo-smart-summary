package db

import (
	"io/fs"
	"strings"
	"testing"

	migrationsql "github.com/Mininglamp-OSS/octo-smart-summary/migrations/sql"
)

// The stale-PR#62 purge allow-list must NEVER contain a migration id that the
// current source actually ships. If it did, purgeStaleMigrationRecords would
// delete a live migration record on every boot and sql-migrate would re-run a
// already-applied DDL (or fail). This test fails loudly if the two sets ever
// intersect, e.g. if a future migration is named like one of the purged ids.
func TestStalePR62MigrationIDs_DoNotShadowShippedMigrations(t *testing.T) {
	shipped := map[string]struct{}{}
	err := fs.WalkDir(migrationsql.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".sql") {
			shipped[path] = struct{}{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded migrations: %v", err)
	}

	for _, id := range stalePR62MigrationIDs {
		if _, ok := shipped[id]; ok {
			t.Errorf("purge allow-list id %q is also a shipped migration; purging it would corrupt migration state", id)
		}
	}
}

// Sanity: the allow-list has no duplicates (a duplicate would be a copy/paste
// slip and harmless, but signals an editing mistake worth catching).
func TestStalePR62MigrationIDs_NoDuplicates(t *testing.T) {
	seen := map[string]struct{}{}
	for _, id := range stalePR62MigrationIDs {
		if _, ok := seen[id]; ok {
			t.Errorf("duplicate id in allow-list: %q", id)
		}
		seen[id] = struct{}{}
	}
}
