//go:build cgo

package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	sqlite3 "github.com/mattn/go-sqlite3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// archiveDriverOnce registers a sqlite3 driver variant that knows the MySQL
// collation name (utf8mb4_unicode_ci) hardcoded into the pipeline's thread
// queries, so those raw SQL joins run under SQLite in tests.
var archiveDriverOnce sync.Once

const archiveDriverName = "sqlite3_agent_collate"

func registerArchiveDriver() {
	archiveDriverOnce.Do(func() {
		sql.Register(archiveDriverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				// Map the MySQL collation to a plain byte comparison.
				return conn.RegisterCollation("utf8mb4_unicode_ci", func(a, b string) int {
					switch {
					case a < b:
						return -1
					case a > b:
						return 1
					default:
						return 0
					}
				})
			},
		})
	})
}

// setupAgentImDB builds an in-memory IM schema sufficient for GetUserChannels,
// following the same pattern as fetch_archive_test.go's setupPipelineImDB.
func setupAgentImDB(t *testing.T) *gorm.DB {
	t.Helper()
	registerArchiveDriver()
	db, err := gorm.Open(sqlite.Dialector{DriverName: archiveDriverName, DSN: ":memory:"}, &gorm.Config{})
	if err != nil {
		t.Fatalf("open agent im db: %v", err)
	}
	// Pin to a single connection: a bare ":memory:" SQLite DB is per-connection.
	if sqlDB, e := db.DB(); e == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	// Schema matching what GetUserChannels expects
	db.Exec(`CREATE TABLE "group" (group_no TEXT NOT NULL, name TEXT, space_id TEXT, status INTEGER DEFAULT 1, creator TEXT, updated_at INTEGER DEFAULT 0)`)
	db.Exec(`CREATE TABLE thread (id INTEGER PRIMARY KEY, short_id TEXT, name TEXT, group_no TEXT, status INTEGER DEFAULT 1, message_count INTEGER DEFAULT 0, creator_uid TEXT, updated_at INTEGER DEFAULT 0)`)
	db.Exec(`CREATE TABLE thread_member (thread_id INTEGER NOT NULL, uid TEXT NOT NULL)`)
	db.Exec(`CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0, role INTEGER DEFAULT 0)`)
	db.Exec(`CREATE TABLE conversation_extra (uid TEXT, channel_id TEXT, channel_type INTEGER, updated_at INTEGER DEFAULT 0)`)
	return db
}

// TestFetchChannelTool_AccessControl tests the actual access control logic
// using a real SQLite in-memory database and real tool handlers.
func TestFetchChannelTool_AccessControl(t *testing.T) {
	// Setup in-memory SQLite database with proper schema
	db := setupAgentImDB(t)

	// Insert test data: user-1 has access to channel-A only
	db.Exec(`INSERT INTO "group" (group_no, name, space_id, status, creator) VALUES ('channel-A', 'Group A', 'test-space', 1, 'user-1')`)
	db.Exec(`INSERT INTO group_member (group_no, uid, is_deleted, role) VALUES ('channel-A', 'user-1', 0, 0)`)

	// Create minimal config
	cfg := config.Config{
		LLMApiURL: "http://test-llm",
		LLMApiKey: "test-key",
		LLMModel:  "test-model",
	}

	// Inject dependencies
	SetSummaryDeps(nil, db, nil, cfg)
	defer func() {
		// Clean up
		SetSummaryDeps(nil, nil, nil, config.Config{})
	}()

	// Get the real tool handler
	toolObj, handler := FetchChannelTool()
	if toolObj.Function.Name != "fetch_channel" {
		t.Fatalf("Expected tool name 'fetch_channel', got %s", toolObj.Function.Name)
	}

	ctx := context.Background()
	testUID := "user-1"

	t.Run("AccessDenied_ChannelNotInAllowedSet", func(t *testing.T) {
		// Request channel-B (not in user-1's accessible channels)
		reqJSON := []byte(`{
			"channel_id": "channel-B",
			"channel_type": 2,
			"time_start": "2024-01-01T00:00:00Z",
			"time_end": "2024-01-02T00:00:00Z"
		}`)

		// Add uid to context using the typed key
		ctxWithUID := context.WithValue(ctx, ContextKeyUID, testUID)

		result, err := handler(ctxWithUID, reqJSON)

		// Should return error indicating access denied
		if err == nil {
			t.Error("Expected error for inaccessible channel, got nil")
		}
		if !strings.Contains(result, "channel not accessible") && !strings.Contains(result, "error") {
			t.Errorf("Expected 'channel not accessible' in result, got: %s", result)
		}
		// Verify the error message mentions the access issue
		if err != nil && !strings.Contains(err.Error(), "not accessible") {
			t.Errorf("Error should mention access denial, got: %s", err.Error())
		}
	})

	t.Run("AccessGranted_ChannelInAllowedSet", func(t *testing.T) {
		// Request channel-A (in user-1's accessible channels)
		reqJSON := []byte(`{
			"channel_id": "channel-A",
			"channel_type": 2,
			"time_start": "2024-01-01T00:00:00Z",
			"time_end": "2024-01-02T00:00:00Z"
		}`)

		// Add uid to context using the typed key
		ctxWithUID := context.WithValue(ctx, ContextKeyUID, testUID)

		result, err := handler(ctxWithUID, reqJSON)

		// Should NOT be rejected by access control
		// (may still fail on message fetch, but should pass access check)
		if err != nil && strings.Contains(result, "channel not accessible") {
			t.Errorf("Channel-A should be accessible, but got access denied: %s", result)
		}

		// If there's an error, it should be about missing message tables, not access
		if err != nil && strings.Contains(err.Error(), "not accessible") {
			t.Errorf("Should not fail on access control, got: %s", err.Error())
		}

		// Verify result is valid JSON (if no error or expected error)
		if err == nil || !strings.Contains(err.Error(), "query messages") {
			var resultMap map[string]interface{}
			if jsonErr := json.Unmarshal([]byte(result), &resultMap); jsonErr != nil {
				t.Logf("Result parse (expected if missing message tables): %v, result: %s", jsonErr, result)
			}
		}
	})
}

// TestPeekChannelTool_AccessControl tests peek_channel access control
func TestPeekChannelTool_AccessControl(t *testing.T) {
	// Setup in-memory SQLite database with proper schema
	db := setupAgentImDB(t)

	// Insert test data: user-2 has access to channel-X only
	db.Exec(`INSERT INTO "group" (group_no, name, space_id, status, creator) VALUES ('channel-X', 'Group X', 'test-space', 1, 'user-2')`)
	db.Exec(`INSERT INTO group_member (group_no, uid, is_deleted, role) VALUES ('channel-X', 'user-2', 0, 0)`)

	// Create minimal config
	cfg := config.Config{
		LLMApiURL: "http://test-llm",
		LLMApiKey: "test-key",
		LLMModel:  "test-model",
	}

	// Inject dependencies
	SetSummaryDeps(nil, db, nil, cfg)
	defer func() {
		SetSummaryDeps(nil, nil, nil, config.Config{})
	}()

	// Get the real tool handler
	toolObj, handler := PeekChannelTool()
	if toolObj.Function.Name != "peek_channel" {
		t.Fatalf("Expected tool name 'peek_channel', got %s", toolObj.Function.Name)
	}

	ctx := context.Background()
	testUID := "user-2"

	t.Run("AccessDenied_ChannelNotInAllowedSet", func(t *testing.T) {
		// Request channel-Y (not in user-2's accessible channels)
		reqJSON := []byte(`{
			"channel_id": "channel-Y",
			"channel_type": 2,
			"time_start": "2024-01-01T00:00:00Z",
			"time_end": "2024-01-02T00:00:00Z",
			"limit": 30
		}`)

		// Add uid to context using the typed key
		ctxWithUID := context.WithValue(ctx, ContextKeyUID, testUID)

		result, err := handler(ctxWithUID, reqJSON)

		// Should return error indicating access denied
		if err == nil {
			t.Error("Expected error for inaccessible channel, got nil")
		}
		if !strings.Contains(result, "channel not accessible") && !strings.Contains(result, "error") {
			t.Errorf("Expected 'channel not accessible' in result, got: %s", result)
		}
		// Verify the error message mentions the access issue
		if err != nil && !strings.Contains(err.Error(), "not accessible") {
			t.Errorf("Error should mention access denial, got: %s", err.Error())
		}
	})

	t.Run("AccessGranted_ChannelInAllowedSet", func(t *testing.T) {
		// Request channel-X (in user-2's accessible channels)
		reqJSON := []byte(`{
			"channel_id": "channel-X",
			"channel_type": 2,
			"time_start": "2024-01-01T00:00:00Z",
			"time_end": "2024-01-02T00:00:00Z",
			"limit": 30
		}`)

		// Add uid to context using the typed key
		ctxWithUID := context.WithValue(ctx, ContextKeyUID, testUID)

		result, err := handler(ctxWithUID, reqJSON)

		// Should NOT be rejected by access control
		if err != nil && strings.Contains(result, "channel not accessible") {
			t.Errorf("Channel-X should be accessible, but got access denied: %s", result)
		}

		// If there's an error, it should be about missing message tables, not access
		if err != nil && strings.Contains(err.Error(), "not accessible") {
			t.Errorf("Should not fail on access control, got: %s", err.Error())
		}

		// Verify result is valid JSON (if no error or expected error)
		if err == nil || !strings.Contains(err.Error(), "query messages") {
			var resultMap map[string]interface{}
			if jsonErr := json.Unmarshal([]byte(result), &resultMap); jsonErr != nil {
				t.Logf("Result parse (expected if missing message tables): %v, result: %s", jsonErr, result)
			}
		}
	})
}
