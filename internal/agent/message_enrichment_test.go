//go:build cgo
// +build cgo

package agent

import (
	"context"
	"database/sql"
	"log"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestEnrichMessagesWithMetadata_PopulatesAllFields tests that enrichment
// correctly fills SenderName, SourceName, and ChannelType — catching the
// SUM-46 Blocker A regression where citations had empty metadata.
//
// This test DOES NOT use fully-populated fake messages. It:
// 1. Creates messages with ONLY the 5 fields pipeline.FetchMessagesFromChannel fills
// 2. Mocks the user table for batch name resolution
// 3. Passes real ChannelInfo from GetUserChannels pattern
// 4. Asserts all 3 metadata fields are populated after enrichment
func TestEnrichMessagesWithMetadata_PopulatesAllFields(t *testing.T) {
	// Setup: in-memory SQLite with user table
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}

	// Create user table matching production schema
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get sql.DB: %v", err)
	}
	defer sqlDB.Close()

	_, err = sqlDB.Exec(`
		CREATE TABLE user (
			uid TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			robot INTEGER NOT NULL DEFAULT 0
		)
	`)
	if err != nil {
		t.Fatalf("failed to create user table: %v", err)
	}
	// robot table must exist too — enrichment unions user.robot with the
	// robot table to match candidates.go's judgement.
	_, err = sqlDB.Exec(`CREATE TABLE robot (robot_id TEXT PRIMARY KEY)`)
	if err != nil {
		t.Fatalf("failed to create robot table: %v", err)
	}

	// Insert test users
	_, err = sqlDB.Exec(`
		INSERT INTO user (uid, name, robot) VALUES
		('u_alice', '张三', 0),
		('u_bob', '李四', 0),
		('u_charlie', '王五', 0)
	`)
	if err != nil {
		t.Fatalf("failed to insert test users: %v", err)
	}

	// 1. Create messages with ONLY the 5 fields pipeline fills
	// (mimics real pipeline.FetchMessagesFromChannel output)
	messages := []pipeline.Message{
		{
			MessageSeq: 101,
			SenderUID:  "u_alice",
			ChannelID:  "group_001",
			Timestamp:  1705123456,
			SendTime:   "2024-01-13T10:20:30Z",
			Content:    "Hello team",
			// SenderName: "",         // NOT filled by pipeline
			// SourceName: "",         // NOT filled by pipeline
			// ChannelType: 0,         // NOT filled by pipeline
		},
		{
			MessageSeq: 102,
			SenderUID:  "u_bob",
			ChannelID:  "group_001",
			Timestamp:  1705123460,
			SendTime:   "2024-01-13T10:20:34Z",
			Content:    "Good morning",
		},
		{
			MessageSeq: 103,
			SenderUID:  "u_charlie",
			ChannelID:  "group_001",
			Timestamp:  1705123465,
			SendTime:   "2024-01-13T10:20:39Z",
			Content:    "Let's start",
		},
	}

	// 2. Provide accessibleChannels (mimics tool security check result)
	accessibleChannels := []pipeline.ChannelInfo{
		{
			ChannelID:   "group_001",
			ChannelType: 2, // Group
			ChannelName: "Development Team",
			SpaceID:     "space_xyz",
		},
		{
			ChannelID:   "group_002",
			ChannelType: 2,
			ChannelName: "QA Team",
		},
	}

	// 3. Call enrichMessagesWithMetadata
	ctx := context.Background()
	enrichMessagesWithMetadata(ctx, messages, "group_001", accessibleChannels, db)

	// 4. Assert all 3 metadata fields are now populated
	for i, msg := range messages {
		// SenderName should be resolved from user table
		if msg.SenderName == "" {
			t.Errorf("message[%d].SenderName is empty (SenderUID=%s)", i, msg.SenderUID)
		}
		expectedNames := map[string]string{
			"u_alice":   "张三",
			"u_bob":     "李四",
			"u_charlie": "王五",
		}
		if expectedName, ok := expectedNames[msg.SenderUID]; ok {
			if msg.SenderName != expectedName {
				t.Errorf("message[%d].SenderName = %q, want %q", i, msg.SenderName, expectedName)
			}
		}

		// SourceName should match channel name
		if msg.SourceName != "Development Team" {
			t.Errorf("message[%d].SourceName = %q, want %q", i, msg.SourceName, "Development Team")
		}

		// ChannelType should be 2 (Group)
		if msg.ChannelType != 2 {
			t.Errorf("message[%d].ChannelType = %d, want 2", i, msg.ChannelType)
		}
	}

	log.Printf("[test] enrichment correctly populated all metadata fields")
}

// TestEnrichMessagesWithMetadata_BatchResolveNoPlusOne verifies that user
// name resolution is done in a single batch query, not per-message (N+1 prevention).
func TestEnrichMessagesWithMetadata_BatchResolveNoPlusOne(t *testing.T) {
	// This test uses query logging to verify batch behavior
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	})
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get sql.DB: %v", err)
	}
	defer sqlDB.Close()

	_, err = sqlDB.Exec(`CREATE TABLE user (uid TEXT PRIMARY KEY, name TEXT NOT NULL, robot INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		t.Fatalf("failed to create user table: %v", err)
	}
	_, err = sqlDB.Exec(`CREATE TABLE robot (robot_id TEXT PRIMARY KEY)`)
	if err != nil {
		t.Fatalf("failed to create robot table: %v", err)
	}
	for i := 0; i < 100; i++ {
		uid := sql.NullString{String: "u_" + string(rune(i)), Valid: true}
		name := sql.NullString{String: "User" + string(rune(i)), Valid: true}
		_, err = sqlDB.Exec(`INSERT INTO user (uid, name) VALUES (?, ?)`, uid, name)
		if err != nil {
			t.Fatalf("failed to insert user: %v", err)
		}
	}

	// Create messages with 100 unique senders
	messages := make([]pipeline.Message, 100)
	for i := 0; i < 100; i++ {
		messages[i] = pipeline.Message{
			MessageSeq: int64(i + 1),
			SenderUID:  "u_" + string(rune(i)),
			ChannelID:  "group_large",
			Timestamp:  int64(1705123456 + i),
			SendTime:   "2024-01-13T10:20:30Z",
			Content:    "Message " + string(rune(i)),
		}
	}

	channels := []pipeline.ChannelInfo{
		{ChannelID: "group_large", ChannelType: 2, ChannelName: "Large Group"},
	}

	// Wrap db to count queries
	queryCount := 0
	db = db.Session(&gorm.Session{
		Logger: logger.Default.LogMode(logger.Info),
	})

	// Track SELECT queries. enrichMessagesWithMetadata builds the user-name
	// lookup with `.Raw(...).Scan(...)`, which fires gorm's Row callback
	// (not Query). Additionally the Before-phase runs before Statement.SQL
	// is rendered, so it's always empty there — we must attach After. The
	// prior Before("gorm:query") registration matched neither the callback
	// class nor the phase, so the counter never incremented and the
	// assertion silently measured 0 for both the batch and hypothetical
	// N+1 case.
	db.Callback().Row().After("gorm:row").Register("count_queries", func(db *gorm.DB) {
		if db.Statement != nil {
			sql := db.Statement.SQL.String()
			if len(sql) >= 6 && sql[0:6] == "SELECT" {
				queryCount++
			}
		}
	})

	ctx := context.Background()
	enrichMessagesWithMetadata(ctx, messages, "group_large", channels, db)

	// We expect EXACTLY 2 batch queries — one against user, one against
	// robot — for all 100 users. Both are O(1) w.r.t. message count. If
	// we see anywhere near 100 queries, that's N+1 and the test should fail.
	if queryCount != 2 {
		t.Errorf("expected 2 batch queries (user + robot), got %d (N+1 detected!)", queryCount)
	}
}

// TestEnrichMessagesWithMetadata_MissingUserGraceful tests that missing users
// don't cause failures — SenderName stays empty but enrichment continues.
func TestEnrichMessagesWithMetadata_MissingUserGraceful(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get sql.DB: %v", err)
	}
	defer sqlDB.Close()

	_, err = sqlDB.Exec(`CREATE TABLE user (uid TEXT PRIMARY KEY, name TEXT NOT NULL, robot INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		t.Fatalf("failed to create user table: %v", err)
	}
	_, err = sqlDB.Exec(`CREATE TABLE robot (robot_id TEXT PRIMARY KEY)`)
	if err != nil {
		t.Fatalf("failed to create robot table: %v", err)
	}

	// Only insert one user
	_, err = sqlDB.Exec(`INSERT INTO user (uid, name, robot) VALUES ('u_alice', '张三', 0)`)
	if err != nil {
		t.Fatalf("failed to insert test user: %v", err)
	}

	messages := []pipeline.Message{
		{SenderUID: "u_alice", ChannelID: "group_001", Content: "Found user"},
		{SenderUID: "u_ghost", ChannelID: "group_001", Content: "Missing user"}, // not in DB
	}

	channels := []pipeline.ChannelInfo{
		{ChannelID: "group_001", ChannelType: 2, ChannelName: "Test Group"},
	}

	ctx := context.Background()
	enrichMessagesWithMetadata(ctx, messages, "group_001", channels, db)

	// u_alice should have name
	if messages[0].SenderName != "张三" {
		t.Errorf("message[0].SenderName = %q, want %q", messages[0].SenderName, "张三")
	}

	// u_ghost won't have name (not found in DB), but enrichment should not crash
	// and SourceName/ChannelType should still be populated
	if messages[1].SourceName != "Test Group" {
		t.Errorf("message[1].SourceName = %q, want %q (should populate even when user missing)",
			messages[1].SourceName, "Test Group")
	}
	if messages[1].ChannelType != 2 {
		t.Errorf("message[1].ChannelType = %d, want 2", messages[1].ChannelType)
	}

	log.Printf("[test] gracefully handled missing user (no crash)")
}

// TestEnrichMessagesWithMetadata_SenderIsBotUnion pins the SenderIsBot
// judgement to match candidates.go's rule: union of (user.robot=1) OR
// (uid IN robot table). A regression that drops either source would break
// bot filtering silently — this test guards both sides plus the negative
// (a plain human user stays SenderIsBot=false).
func TestEnrichMessagesWithMetadata_SenderIsBotUnion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get sql.DB: %v", err)
	}
	defer sqlDB.Close()

	_, err = sqlDB.Exec(`CREATE TABLE user (uid TEXT PRIMARY KEY, name TEXT NOT NULL, robot INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		t.Fatalf("failed to create user table: %v", err)
	}
	_, err = sqlDB.Exec(`CREATE TABLE robot (robot_id TEXT PRIMARY KEY)`)
	if err != nil {
		t.Fatalf("failed to create robot table: %v", err)
	}

	// Four users covering all legs of the union PLUS the empty-name bot
	// regression that motivated the fix (PR #237 review, P1 blocking):
	//   u_human       — user row with robot=0, absent from robot table     ⇒ false
	//   u_flagged     — user row with robot=1                                ⇒ true (leg 1)
	//   u_system      — user row with robot=0, present in robot table       ⇒ true (leg 2)
	//   u_bot_noname  — user row with robot=1 AND name='' (programmatically
	//                   provisioned bot); must NOT be filtered out at SQL
	//                   layer or the classification silently drops.         ⇒ true (leg 3)
	_, err = sqlDB.Exec(`
		INSERT INTO user (uid, name, robot) VALUES
		('u_human',      '张三',      0),
		('u_flagged',    'CI机器人',   1),
		('u_system',     'BotFather', 0),
		('u_bot_noname', '',          1)
	`)
	if err != nil {
		t.Fatalf("failed to seed user rows: %v", err)
	}
	_, err = sqlDB.Exec(`INSERT INTO robot (robot_id) VALUES ('u_system')`)
	if err != nil {
		t.Fatalf("failed to seed robot rows: %v", err)
	}

	messages := []pipeline.Message{
		{SenderUID: "u_human", ChannelID: "g", Content: "hi"},
		{SenderUID: "u_flagged", ChannelID: "g", Content: "PR merged"},
		{SenderUID: "u_system", ChannelID: "g", Content: "system notice"},
		{SenderUID: "u_bot_noname", ChannelID: "g", Content: "auto post"},
	}
	channels := []pipeline.ChannelInfo{
		{ChannelID: "g", ChannelType: 2, ChannelName: "test"},
	}

	enrichMessagesWithMetadata(context.Background(), messages, "g", channels, db)

	want := map[string]bool{
		"u_human":      false,
		"u_flagged":    true,
		"u_system":     true,
		"u_bot_noname": true,
	}
	for i, msg := range messages {
		expected, ok := want[msg.SenderUID]
		if !ok {
			t.Fatalf("unexpected uid %q at index %d", msg.SenderUID, i)
		}
		if msg.SenderIsBot != expected {
			t.Errorf("message[%d] uid=%q: SenderIsBot=%v, want %v",
				i, msg.SenderUID, msg.SenderIsBot, expected)
		}
	}
}
