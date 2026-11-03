package service

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	migrations "github.com/Mininglamp-OSS/octo-smart-summary/migrations/sql"
	_ "github.com/go-sql-driver/mysql"
)

// Opt-in real MySQL migration test: JSON and binary payload guards cannot be
// validated by SQLite. Requires a disposable summary_cr_* database.
func TestPersonalProvenanceUpgradeMigrationMySQL(t *testing.T) {
	dsn := os.Getenv("SUMMARY_CR_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUMMARY_CR_MYSQL_TEST_DSN is not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL failed")
	}
	defer db.Close()
	var name string
	if err := db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil || !strings.HasPrefix(name, "summary_cr_") {
		t.Fatal("requires disposable summary_cr_* database")
	}
	// Temporary tables shadow any persistent names on this connection.
	db.SetMaxOpenConns(1)
	payload := `id BIGINT PRIMARY KEY, task_id BIGINT, user_id VARCHAR(64), content TEXT, citations_json TEXT, msg_count INT, total_token_used INT, model_version VARCHAR(64), snapshot_json TEXT`
	for _, query := range []string{
		"CREATE TEMPORARY TABLE summary_personal_result_version (" + payload + ", version INT)",
		"CREATE TEMPORARY TABLE summary_personal_result (" + payload + ", current_version_id BIGINT)",
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	cases := []string{"matching", "changed-content", "changed-citations", "other-task", "other-user", "different-version", "invalid-json", "known-history", "different-model", "old-history"}
	for i, scenario := range cases {
		id := i + 1
		currentTask := int64(id)
		currentUser := "owner"
		content := "Body"
		citations := "[]"
		modelName := "model"
		raw := fmt.Sprintf(`{"task_id":%d,"content_version":1,"scope":{"channel_ids":["known-source"]}}`, id)
		var oldSnapshot interface{}
		pointer := id
		switch scenario {
		case "changed-content":
			content = "Edited body"
		case "changed-citations":
			citations = `[{"index":1}]`
		case "other-task":
			currentTask = 999
		case "other-user":
			currentUser = "someone-else"
		case "different-version":
			raw = fmt.Sprintf(`{"task_id":%d,"content_version":2}`, id)
		case "invalid-json":
			raw = "not-json"
		case "known-history":
			oldSnapshot = `{"provenance_status":"unknown"}`
		case "different-model":
			modelName = "other-model"
		case "old-history":
			pointer = 9999
		}
		if _, err := db.Exec(`INSERT INTO summary_personal_result_version VALUES (?,?,'owner','Body','[]',5,10,'model',?,1)`, id, id, oldSnapshot); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO summary_personal_result VALUES (?,?,?,?,?,5,10,?,?,?)`, id, currentTask, currentUser, content, citations, modelName, raw, pointer); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := migrations.FS.ReadFile("20261020-02-personal-version-provenance.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := strings.Split(strings.TrimPrefix(string(raw), "-- +migrate Up"), "-- +migrate Down")[0]
	for run := 0; run < 2; run++ {
		if _, err := db.Exec(up); err != nil {
			t.Fatal(err)
		}
		for i, scenario := range cases {
			var snapshot sql.NullString
			if err := db.QueryRow("SELECT snapshot_json FROM summary_personal_result_version WHERE id=?", i+1).Scan(&snapshot); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "matching":
				if !snapshot.Valid || !strings.Contains(snapshot.String, "known-source") {
					t.Fatal("matching provenance not recovered")
				}
			case "known-history":
				if snapshot.String != `{"provenance_status":"unknown"}` {
					t.Fatal("known history overwritten")
				}
			default:
				if snapshot.Valid {
					t.Fatalf("fabricated provenance for %s", scenario)
				}
			}
		}
	}
}
