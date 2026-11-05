package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	migrationsql "github.com/Mininglamp-OSS/octo-smart-summary/migrations/sql"
	"github.com/go-sql-driver/mysql"
	migrate "github.com/rubenv/sql-migrate"
)

// Opt-in, destructive ONLY in a disposable loopback summary_migration_* database.
// Real MySQL is required: SQLite transactions hide MySQL implicit DDL commits.
func migrationRetryDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("SUMMARY_MIGRATION_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUMMARY_MIGRATION_MYSQL_TEST_DSN not set")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid isolated MySQL DSN")
	}
	if cfg.Net != "tcp" || !(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) || !strings.HasPrefix(cfg.DBName, "summary_migration_") {
		t.Fatal("requires loopback TCP and a disposable summary_migration_* database")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open isolated MySQL")
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal("isolated MySQL unavailable")
	}
	return db, dsn
}

func retryMigrations(t *testing.T) []*migrate.Migration {
	t.Helper()
	source := &migrate.EmbedFileSystemMigrationSource{FileSystem: migrationsql.FS, Root: "."}
	all, err := source.FindMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var out []*migrate.Migration
	for _, m := range all {
		if m.Id >= "20261019-01-optimization-save.sql" && m.Id <= "20261021-01-summary-search-normalized.sql" {
			out = append(out, m)
		}
	}
	if len(out) != 4 {
		t.Fatalf("expected four rollout migrations, got %d", len(out))
	}
	return out
}

func resetRetryFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`DROP TABLE IF EXISTS gorp_migrations, summary_agent_save_idempotency, summary_result, summary_personal_result_version, summary_personal_result, summary_task`,
		`CREATE TABLE summary_agent_save_idempotency (id BIGINT PRIMARY KEY, task_id BIGINT NOT NULL, user_id VARCHAR(64) NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE summary_result (id BIGINT PRIMARY KEY, task_id BIGINT NOT NULL, content MEDIUMTEXT) ENGINE=InnoDB`,
		`CREATE TABLE summary_personal_result_version (id BIGINT PRIMARY KEY, task_id BIGINT, user_id VARCHAR(64), version INT, content MEDIUMTEXT, citations_json MEDIUMTEXT, msg_count INT, total_token_used INT, model_version VARCHAR(50)) ENGINE=InnoDB`,
		`CREATE TABLE summary_personal_result (id BIGINT PRIMARY KEY, task_id BIGINT, user_id VARCHAR(64), current_version_id BIGINT, content MEDIUMTEXT, citations_json MEDIUMTEXT, msg_count INT, total_token_used INT, model_version VARCHAR(50), snapshot_json MEDIUMTEXT) ENGINE=InnoDB`,
		`CREATE TABLE summary_task (id BIGINT PRIMARY KEY, title VARCHAR(2300), topic VARCHAR(2300), updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`INSERT INTO summary_result VALUES (1,10,'keep original body')`,
		`INSERT INTO summary_agent_save_idempotency VALUES (1,10,'actor')`,
		`INSERT INTO summary_personal_result_version VALUES (1,10,'actor',1,'body','[]',2,3,'model'),(2,20,'actor',1,'different body','[]',2,3,'model')`,
		`INSERT INTO summary_personal_result VALUES (1,10,'actor',1,'body','[]',2,3,'model','{"task_id":10,"content_version":1}'),(2,20,'actor',2,'body','[]',2,3,'model','{"task_id":20,"content_version":1}')`,
		`INSERT INTO summary_task VALUES (10,'ＡＢＣ','Topic','2026-01-01 00:00:00')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func assertRetryComplete(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM gorp_migrations`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("migration records: %d, %v", count, err)
	}
	var body, owner string
	if err := db.QueryRow(`SELECT content,citation_owner_id FROM summary_result WHERE id=1`).Scan(&body, &owner); err != nil || body != "keep original body" || owner != "" {
		t.Fatalf("legacy data changed: %q %q %v", body, owner, err)
	}
	var known, unknown sql.NullString
	if err := db.QueryRow(`SELECT snapshot_json FROM summary_personal_result_version WHERE id=1`).Scan(&known); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT snapshot_json FROM summary_personal_result_version WHERE id=2`).Scan(&unknown); err != nil {
		t.Fatal(err)
	}
	if !known.Valid || unknown.Valid {
		t.Fatal("provenance guard did not preserve matching/unknown history")
	}
	if _, err := db.Exec(`SELECT result_id,result_version,result_kind FROM summary_agent_save_idempotency`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT snapshot_json FROM summary_result`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='summary_agent_save_idempotency' AND INDEX_NAME='idx_save_result_task_kind'`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("index: %d %v", count, err)
	}
	if err := BackfillSummarySearch(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var title, stamp string
	if err := db.QueryRow(`SELECT search_title,CAST(updated_at AS CHAR) FROM summary_task WHERE id=10`).Scan(&title, &stamp); err != nil || title != "abc" || stamp != "2026-01-01 00:00:00" {
		t.Fatalf("search backfill: %q %q %v", title, stamp, err)
	}
}

func TestRolloutMigrationsRetryMySQL(t *testing.T) {
	db, dsn := migrationRetryDB(t)
	migrations := retryMigrations(t)
	for _, mode := range []string{"statement-error", "lost-connection"} {
		for i, m := range migrations {
			// Include failure after the final SQL, before the migration receipt.
			for cut := 0; cut <= len(m.Up); cut++ {
				t.Run(fmt.Sprintf("%s/%s/after-%02d", mode, m.Id, cut), func(t *testing.T) {
					resetRetryFixture(t, db)
					if i > 0 {
						if _, err := runMigrationsCore(db, "mysql", &migrate.MemoryMigrationSource{Migrations: migrations[:i]}); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "statement-error" {
						broken := *m
						broken.Up = append(append([]string(nil), m.Up[:cut]...), "SELECT * FROM deliberate_migration_failure")
						steps := append(append([]*migrate.Migration(nil), migrations[:i]...), &broken)
						if _, err := runMigrationsCore(db, "mysql", &migrate.MemoryMigrationSource{Migrations: steps}); err == nil {
							t.Fatal("injected failure did not fire")
						}
					} else {
						conn, err := db.Conn(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						var id int64
						if err := conn.QueryRowContext(context.Background(), "SELECT CONNECTION_ID()").Scan(&id); err != nil {
							t.Fatal(err)
						}
						tx, err := conn.BeginTx(context.Background(), nil)
						if err != nil {
							t.Fatal(err)
						}
						for _, q := range m.Up[:cut] {
							if _, err := tx.Exec(q); err != nil {
								t.Fatal(err)
							}
						}
						// A separate server connection kills the migration connection, with no
						// migration receipt. This also discards prepared statements/session vars.
						if _, err := db.Exec(fmt.Sprintf("KILL CONNECTION %d", id)); err != nil {
							t.Fatal(err)
						}
						if _, err := tx.Exec("SELECT 1"); err == nil {
							t.Fatal("connection survived KILL")
						}
						_ = tx.Rollback()
						_ = conn.Close()
					}
					// Reopen as a restarted process would, rather than relying on session state.
					fresh, err := sql.Open("mysql", dsn)
					if err != nil {
						t.Fatal("reopen isolated database")
					}
					defer fresh.Close()
					source := &migrate.MemoryMigrationSource{Migrations: migrations}
					if _, err := runMigrationsCore(fresh, "mysql", source); err != nil {
						t.Fatalf("retry failed: %v", err)
					}
					assertRetryComplete(t, fresh)
					if n, err := runMigrationsCore(fresh, "mysql", source); err != nil || n != 0 {
						t.Fatalf("second retry n=%d err=%v", n, err)
					}
				})
			}
		}
	}
}

func TestRolloutMigrationsRejectIncompatibleSchemaMySQL(t *testing.T) {
	db, _ := migrationRetryDB(t)
	migrations := retryMigrations(t)
	for _, tc := range []struct {
		name, ddl string
		prefix    int
	}{
		{"column", `ALTER TABLE summary_agent_save_idempotency ADD COLUMN result_id VARCHAR(10)`, 0},
		{"index", `CREATE INDEX idx_save_result_task_kind ON summary_agent_save_idempotency (task_id,result_id,result_kind)`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetRetryFixture(t, db)
			if tc.prefix > 0 {
				if _, err := runMigrationsCore(db, "mysql", &migrate.MemoryMigrationSource{Migrations: migrations[:tc.prefix]}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(tc.ddl); err != nil {
				t.Fatal(err)
			}
			if _, err := runMigrationsCore(db, "mysql", &migrate.MemoryMigrationSource{Migrations: migrations}); err == nil {
				t.Fatal("incompatible existing schema silently accepted")
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM gorp_migrations`).Scan(&count); err != nil || count != tc.prefix {
				t.Fatalf("recorded incompatible schema: %d %v", count, err)
			}
		})
	}
}

// Trusted ownership backfill is repeatable and does not overwrite provenance.
func TestCitationOwnerBackfillRetryMySQL(t *testing.T) {
	db, _ := migrationRetryDB(t)
	resetRetryFixture(t, db)
	migrations := retryMigrations(t)
	if _, err := runMigrationsCore(db, "mysql", &migrate.MemoryMigrationSource{Migrations: migrations[:1]}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE summary_agent_save_idempotency SET result_id=1,result_version=1,result_kind='team' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	for _, q := range migrations[1].Up {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// SQL committed, but the migration receipt was never written.
	if _, err := runMigrationsCore(db, "mysql", &migrate.MemoryMigrationSource{Migrations: migrations}); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := db.QueryRow(`SELECT citation_owner_id FROM summary_result WHERE id=1`).Scan(&owner); err != nil || owner != "actor" {
		t.Fatalf("trusted owner: %q %v", owner, err)
	}
	if _, err := db.Exec(`UPDATE summary_agent_save_idempotency SET user_id='changed-binding' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	for _, q := range migrations[1].Up {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(`SELECT citation_owner_id FROM summary_result WHERE id=1`).Scan(&owner); err != nil || owner != "actor" {
		t.Fatalf("overwrote owner: %q %v", owner, err)
	}
}

func TestSearchBackfillRetryMySQL(t *testing.T) {
	db, _ := migrationRetryDB(t)
	resetRetryFixture(t, db)
	if _, err := runMigrationsCore(db, "mysql", &migrate.MemoryMigrationSource{Migrations: retryMigrations(t)}); err != nil {
		t.Fatal(err)
	}
	for i := 100; i < 1101; i++ {
		if _, err := db.Exec(`INSERT INTO summary_task (id,title,topic,updated_at) VALUES (?,'ＡＢＣ','Topic','2026-01-01')`, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_search_batch BEFORE UPDATE ON summary_task FOR EACH ROW BEGIN IF NEW.id=700 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected batch failure'; END IF; END`); err != nil {
		t.Fatal(err)
	}
	if err := BackfillSummarySearch(context.Background(), db); err == nil {
		t.Fatal("expected second-batch failure")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM summary_task WHERE search_title IS NOT NULL`).Scan(&count); err != nil || count != 500 {
		t.Fatalf("first batch durability: %d %v", count, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_search_batch`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := BackfillSummarySearch(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM summary_task WHERE search_title IS NULL OR search_topic IS NULL OR updated_at <> '2026-01-01'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("backfill lost rows or changed timestamps: %d %v", count, err)
	}
}
