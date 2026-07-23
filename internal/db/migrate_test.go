//go:build cgo

package db

import (
	"database/sql"
	"net/http"
	"testing"
	"testing/fstest"

	migrate "github.com/rubenv/sql-migrate"

	_ "github.com/mattn/go-sqlite3"
)

var testFS = fstest.MapFS{
	"20260101-00-baseline.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
CREATE TABLE IF NOT EXISTS summary_chunk (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  chunk_index INTEGER NOT NULL,
  participant_id INTEGER,
  summary_source_id INTEGER,
  msg_count INTEGER NOT NULL DEFAULT 0,
  msg_start_time TEXT,
  msg_end_time TEXT,
  chunk_summary TEXT NOT NULL,
  token_used INTEGER NOT NULL DEFAULT 0,
  status INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS summary_event (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  status INTEGER NOT NULL,
  progress INTEGER NOT NULL DEFAULT 0,
  message TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS summary_participant (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  user_id TEXT NOT NULL,
  user_name TEXT NOT NULL DEFAULT '',
  status INTEGER NOT NULL DEFAULT 0,
  confirmed_at TEXT,
  personal_result_id INTEGER,
  worker_started_at TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS summary_personal_result (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  participant_ref_id INTEGER NOT NULL,
  user_id TEXT NOT NULL,
  content TEXT NOT NULL,
  citations_json TEXT,
  msg_count INTEGER NOT NULL DEFAULT 0,
  total_token_used INTEGER NOT NULL DEFAULT 0,
  model_version TEXT NOT NULL DEFAULT '',
  worker_status INTEGER NOT NULL DEFAULT 0,
  error_message TEXT,
  submitted_at TEXT,
  generated_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS summary_result (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  content TEXT NOT NULL,
  citations_json TEXT,
  team_citations_json TEXT,
  total_msg_count INTEGER NOT NULL DEFAULT 0,
  total_token_used INTEGER NOT NULL DEFAULT 0,
  model_version TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 1,
  generated_at TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS summary_schedule (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  space_id TEXT NOT NULL DEFAULT '',
  creator_id TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  summary_mode INTEGER NOT NULL,
  cron_expr TEXT NOT NULL,
  time_range_type INTEGER NOT NULL,
  source_config TEXT,
  participant_config TEXT,
  is_active INTEGER NOT NULL DEFAULT 1,
  last_run_at TEXT,
  next_run_at TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  deleted_at TEXT
);

CREATE TABLE IF NOT EXISTS summary_source (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  source_type INTEGER NOT NULL,
  source_id TEXT NOT NULL,
  source_name TEXT NOT NULL DEFAULT '',
  participant_id INTEGER,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS summary_task (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_no TEXT NOT NULL UNIQUE,
  space_id TEXT NOT NULL DEFAULT '',
  creator_id TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  summary_mode INTEGER NOT NULL,
  time_range_start TEXT NOT NULL,
  time_range_end TEXT NOT NULL,
  status INTEGER NOT NULL DEFAULT 0,
  trigger_type INTEGER NOT NULL DEFAULT 1,
  retry_count INTEGER NOT NULL DEFAULT 0,
  error_message TEXT,
  schedule_id INTEGER,
  processing_deadline TEXT,
  confirm_deadline TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  deleted_at TEXT
);

-- +migrate Down
DROP TABLE IF EXISTS summary_task;
DROP TABLE IF EXISTS summary_source;
DROP TABLE IF EXISTS summary_schedule;
DROP TABLE IF EXISTS summary_result;
DROP TABLE IF EXISTS summary_personal_result;
DROP TABLE IF EXISTS summary_participant;
DROP TABLE IF EXISTS summary_event;
DROP TABLE IF EXISTS summary_chunk;
`)},
	"20260101-06-batch-indexes.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
CREATE INDEX idx_participant_task_user ON summary_participant (user_id, task_id);
CREATE INDEX idx_event_task_id ON summary_event (task_id, id DESC);

-- +migrate Down
DROP INDEX IF EXISTS idx_participant_task_user;
DROP INDEX IF EXISTS idx_event_task_id;
`)},
	"20260506-01-title-varchar-1000.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
-- SQLite does not support ALTER TABLE MODIFY COLUMN; use a no-op for test
SELECT 1;

-- +migrate Down
SELECT 1;
`)},
	"20260703-01-add-personal-workflow-stage.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
ALTER TABLE summary_personal_result ADD COLUMN workflow_stage varchar(32) NOT NULL DEFAULT '';

-- +migrate Down
ALTER TABLE summary_personal_result DROP COLUMN workflow_stage;
`)},
	"20260706-01-summary-user-template.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
CREATE TABLE summary_user_template (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  space_id TEXT NOT NULL DEFAULT '',
  user_id TEXT NOT NULL,
  template_id TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  is_custom INTEGER NOT NULL DEFAULT 0,
  pattern TEXT NOT NULL,
  sort_order INTEGER NOT NULL DEFAULT 0,
  deleted_at TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX uk_summary_user_template ON summary_user_template (space_id, user_id, template_id);
CREATE INDEX idx_summary_user_template_user ON summary_user_template (space_id, user_id, is_custom, deleted_at);

-- +migrate Down
DROP TABLE IF EXISTS summary_user_template;
`)},
	"20260707-01-summary-user-template-compat.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
SELECT 1;

-- +migrate Down
SELECT 1;
`)},
	"20260720-01-summary-user-template-description-1000.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
SELECT 1;

-- +migrate Down
SELECT 1;
`)},
	"20260720-02-summary-title-varchar-1300.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
SELECT 1;

-- +migrate Down
SELECT 1;
`)},
	"20260720-03-summary-task-topic.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
SELECT 1;

-- +migrate Down
SELECT 1;
`)},
	"20260721-01-summary-content-limits-2000.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
SELECT 1;

-- +migrate Down
SELECT 1;
`)},
	"20260721-02-summary-share-snapshot.sql": &fstest.MapFile{Data: []byte(`-- +migrate Up
CREATE TABLE summary_share_snapshot (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  task_no TEXT NOT NULL,
  space_id TEXT NOT NULL,
  creator_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  source_name TEXT NOT NULL DEFAULT '',
  source_count INTEGER NOT NULL DEFAULT 0,
  participant_count INTEGER NOT NULL DEFAULT 0,
  message_count INTEGER NOT NULL DEFAULT 0,
  time_range_start TEXT NOT NULL,
  time_range_end TEXT NOT NULL,
  summary_mode INTEGER NOT NULL,
  result_version INTEGER NOT NULL DEFAULT 1,
  preview TEXT NOT NULL,
  content TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX uk_summary_share_idempotency ON summary_share_snapshot (space_id, creator_id, idempotency_key);
CREATE INDEX idx_summary_share_task ON summary_share_snapshot (task_id);
CREATE TABLE summary_share_grant (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  snapshot_id INTEGER NOT NULL,
  share_id TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  channel_type INTEGER NOT NULL,
  status INTEGER NOT NULL DEFAULT 1,
  revoked_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX uk_summary_share_id ON summary_share_grant (share_id);
CREATE UNIQUE INDEX uk_summary_share_target ON summary_share_grant (snapshot_id, channel_id, channel_type);

-- +migrate Down
DROP TABLE IF EXISTS summary_share_grant;
DROP TABLE IF EXISTS summary_share_snapshot;
`)},
}

func testSource() migrate.MigrationSource {
	return &migrate.HttpFileSystemMigrationSource{
		FileSystem: http.FS(testFS),
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRunMigrations_NewDB(t *testing.T) {
	db := openTestDB(t)

	n, err := runMigrationsCore(db, "sqlite3", testSource())
	if err != nil {
		t.Fatalf("runMigrationsCore: %v", err)
	}
	if n != 11 {
		t.Fatalf("expected 11 migrations applied, got %d", n)
	}

	tables := []string{
		"summary_chunk", "summary_event", "summary_participant",
		"summary_personal_result", "summary_result", "summary_schedule",
		"summary_source", "summary_task", "summary_user_template",
		"summary_share_snapshot", "summary_share_grant",
	}
	for _, tbl := range tables {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s not found: %v", tbl, err)
		}
	}

	indexes := []string{"idx_participant_task_user", "idx_event_task_id", "uk_summary_user_template", "idx_summary_user_template_user"}
	for _, idx := range indexes {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='index' AND name=?", idx).Scan(&name)
		if err != nil {
			t.Errorf("index %s not found: %v", idx, err)
		}
	}

	var workflowStageCol string
	err = db.QueryRow("SELECT name FROM pragma_table_info('summary_personal_result') WHERE name='workflow_stage'").Scan(&workflowStageCol)
	if err != nil {
		t.Fatalf("workflow_stage column not found: %v", err)
	}

	for _, col := range []string{"label", "description", "is_custom", "sort_order", "deleted_at"} {
		var name string
		err = db.QueryRow("SELECT name FROM pragma_table_info('summary_user_template') WHERE name=?", col).Scan(&name)
		if err != nil {
			t.Fatalf("summary_user_template column %s not found: %v", col, err)
		}
	}
}

func TestRunMigrations_ExistingDB(t *testing.T) {
	db := openTestDB(t)

	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS gorp_migrations (
		id TEXT NOT NULL PRIMARY KEY,
		applied_at DATETIME NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create gorp_migrations: %v", err)
	}
	_, err = db.Exec(`INSERT INTO gorp_migrations (id, applied_at) VALUES ('20260101-00-baseline.sql', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed baseline: %v", err)
	}

	stmts := []string{
		`CREATE TABLE summary_chunk (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE summary_event (id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL)`,
		`CREATE TABLE summary_participant (id INTEGER PRIMARY KEY, user_id TEXT NOT NULL, task_id INTEGER NOT NULL)`,
		`CREATE TABLE summary_personal_result (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE summary_result (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE summary_schedule (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE summary_source (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE summary_task (id INTEGER PRIMARY KEY)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("pre-create table: %v", err)
		}
	}

	n, err := runMigrationsCore(db, "sqlite3", testSource())
	if err != nil {
		t.Fatalf("runMigrationsCore: %v", err)
	}
	if n != 10 {
		t.Fatalf("expected 10 migrations applied (including summary share snapshot/grant), got %d", n)
	}
}

func TestRunMigrations_Skip(t *testing.T) {
	t.Setenv("SKIP_MIGRATION", "true")

	db := openTestDB(t)

	n, err := RunMigrations(db)
	if err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 migrations (skipped), got %d", n)
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	db := openTestDB(t)

	n1, err := runMigrationsCore(db, "sqlite3", testSource())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if n1 != 11 {
		t.Fatalf("first run: expected 11, got %d", n1)
	}

	n2, err := runMigrationsCore(db, "sqlite3", testSource())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second run: expected 0, got %d", n2)
	}
}
