package pipeline

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// archiveDriverOnce registers a sqlite3 driver variant that knows the MySQL
// collation name (utf8mb4_unicode_ci) hardcoded into the pipeline's thread
// queries, so those raw SQL joins run under SQLite in tests.
var archiveDriverOnce sync.Once

const archiveDriverName = "sqlite3_pipeline_collate"

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

// setupPipelineImDB builds an in-memory IM schema sufficient for GetUserChannels
// and the message-fetch path, with the MySQL collation registered.
func setupPipelineImDB(t *testing.T) *gorm.DB {
	t.Helper()
	registerArchiveDriver()
	db, err := gorm.Open(sqlite.Dialector{DriverName: archiveDriverName, DSN: ":memory:"}, &gorm.Config{})
	if err != nil {
		t.Fatalf("open pipeline im db: %v", err)
	}
	db.Exec(`CREATE TABLE "group" (group_no TEXT NOT NULL, name TEXT, space_id TEXT, status INTEGER DEFAULT 1, creator TEXT, updated_at INTEGER DEFAULT 0)`)
	db.Exec(`CREATE TABLE thread (id INTEGER PRIMARY KEY, short_id TEXT, name TEXT, group_no TEXT, status INTEGER DEFAULT 1, message_count INTEGER DEFAULT 0, creator_uid TEXT, updated_at INTEGER DEFAULT 0)`)
	db.Exec(`CREATE TABLE thread_member (thread_id INTEGER NOT NULL, uid TEXT NOT NULL)`)
	db.Exec(`CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0, role INTEGER DEFAULT 0)`)
	db.Exec(`CREATE TABLE conversation_extra (uid TEXT, channel_id TEXT, channel_type INTEGER, updated_at INTEGER DEFAULT 0)`)
	// Single message shard ("message") is enough for tableCount=1.
	db.Exec("CREATE TABLE message (message_seq INTEGER, from_uid TEXT, channel_id TEXT, channel_type INTEGER, timestamp INTEGER, payload BLOB, is_deleted INTEGER DEFAULT 0)")
	return db
}

// seedThreadScenario seeds one active group with Active(1)/Archived(2)/Deleted(3)
// threads, makes user1 a member of each, and inserts one text message per thread.
func seedPipelineThreads(db *gorm.DB, ts int64) {
	db.Exec(`INSERT INTO "group" (group_no, name, space_id, status, creator) VALUES ('grp1', 'TestGroup', 'space1', 1, 'user1')`)
	db.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count, creator_uid) VALUES (1, 'thA', 'Active', 'grp1', 1, 5, 'user1')`)
	db.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count, creator_uid) VALUES (2, 'thB', 'Archived', 'grp1', 2, 4, 'user1')`)
	db.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count, creator_uid) VALUES (3, 'thC', 'Deleted', 'grp1', 3, 9, 'user1')`)
	db.Exec(`INSERT INTO thread_member (thread_id, uid) VALUES (1, 'user1'), (2, 'user1'), (3, 'user1')`)
	db.Exec(`INSERT INTO group_member (group_no, uid, is_deleted, role) VALUES ('grp1', 'user1', 0, 0)`)

	payload := `{"type":1,"content":"hello"}`
	db.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		1, "user1", "grp1____thA", 5, ts, []byte(payload), 0)
	db.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		2, "user1", "grp1____thB", 5, ts, []byte(payload), 0)
	db.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		3, "user1", "grp1____thC", 5, ts, []byte(payload), 0)
}

func threadIDSet(channels []ChannelInfo) map[string]bool {
	set := map[string]bool{}
	for _, ch := range channels {
		if ch.ChannelType == 5 {
			set[ch.ChannelID] = true
		}
	}
	return set
}

func TestGetUserChannels_NoSelection_ExcludesArchivedAndDeleted(t *testing.T) {
	db := setupPipelineImDB(t)
	seedPipelineThreads(db, time.Now().Unix())

	channels, err := GetUserChannels(context.Background(), "user1", db)
	if err != nil {
		t.Fatalf("GetUserChannels: %v", err)
	}
	threads := threadIDSet(channels)
	if !threads["grp1____thA"] {
		t.Errorf("active thread should be discovered, got %v", threads)
	}
	if threads["grp1____thB"] {
		t.Errorf("archived thread must NOT be auto-discovered, got %v", threads)
	}
	if threads["grp1____thC"] {
		t.Errorf("deleted thread must never be discovered, got %v", threads)
	}
}

func TestGetUserChannels_SelectedArchivedRetained(t *testing.T) {
	db := setupPipelineImDB(t)
	seedPipelineThreads(db, time.Now().Unix())

	channels, err := GetUserChannels(context.Background(), "user1", db, "grp1____thB")
	if err != nil {
		t.Fatalf("GetUserChannels: %v", err)
	}
	threads := threadIDSet(channels)
	if !threads["grp1____thA"] {
		t.Errorf("active thread should still be discovered, got %v", threads)
	}
	if !threads["grp1____thB"] {
		t.Errorf("explicitly-selected archived thread must be retained, got %v", threads)
	}
	// Even when an archived thread is selected, Deleted (3) must stay excluded.
	if threads["grp1____thC"] {
		t.Errorf("deleted thread must never be discovered even when selected siblings exist, got %v", threads)
	}
}

func TestGetUserChannels_SelectedDeletedNeverRetained(t *testing.T) {
	db := setupPipelineImDB(t)
	seedPipelineThreads(db, time.Now().Unix())

	// Even if a deleted thread id is passed as selected, it must not appear:
	// the relaxation only admits status=2.
	channels, err := GetUserChannels(context.Background(), "user1", db, "grp1____thC")
	if err != nil {
		t.Fatalf("GetUserChannels: %v", err)
	}
	threads := threadIDSet(channels)
	if threads["grp1____thC"] {
		t.Errorf("deleted thread must never be discovered, got %v", threads)
	}
}

func TestResolveAndFetch_SelectedArchivedThread_ProducesMessages(t *testing.T) {
	db := setupPipelineImDB(t)
	ts := time.Now().Add(-time.Hour).Unix()
	seedPipelineThreads(db, ts)

	specifiedSources := []map[string]interface{}{
		{"source_id": "grp1____thB", "source_type": 2}, // frontend thread type
	}

	start := time.Now().Add(-24 * time.Hour)
	end := time.Now().Add(time.Hour)

	msgs, err := ResolveAndFetchMessagesForPersonal(
		context.Background(), "user1", nil, nil, specifiedSources, "",
		start, end, db, nil, nil, 1, 0, 2, nil,
	)
	if err != nil {
		t.Fatalf("ResolveAndFetchMessagesForPersonal: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatalf("expected non-empty summary input for selected archived thread, got 0 messages")
	}
	for _, m := range msgs {
		if m.ChannelID != "grp1____thB" {
			t.Errorf("unexpected message from %s, want only grp1____thB", m.ChannelID)
		}
	}
}

func TestResolveAndFetch_NoSources_ArchivedExcluded(t *testing.T) {
	db := setupPipelineImDB(t)
	ts := time.Now().Add(-time.Hour).Unix()
	seedPipelineThreads(db, ts)

	start := time.Now().Add(-24 * time.Hour)
	end := time.Now().Add(time.Hour)

	// No explicit sources -> auto discovery -> only the active thread's message.
	msgs, err := ResolveAndFetchMessagesForPersonal(
		context.Background(), "user1", nil, nil, nil, "",
		start, end, db, nil, nil, 1, 0, 2, nil,
	)
	if err != nil {
		t.Fatalf("ResolveAndFetchMessagesForPersonal: %v", err)
	}
	for _, m := range msgs {
		if m.ChannelID == "grp1____thB" {
			t.Errorf("archived thread message leaked into auto summary: %+v", m)
		}
		if m.ChannelID == "grp1____thC" {
			t.Errorf("deleted thread message must never be fetched: %+v", m)
		}
	}
	// The active thread should be present.
	var sawActive bool
	for _, m := range msgs {
		if m.ChannelID == "grp1____thA" {
			sawActive = true
		}
	}
	if !sawActive {
		t.Errorf("expected active thread message in auto summary, got %d messages", len(msgs))
	}
}

func TestFilterByOwnership_ArchivedThreadCreatorRetained(t *testing.T) {
	db := setupPipelineImDB(t)
	seedPipelineThreads(db, time.Now().Unix())

	// user1 is creator of both active (thA) and archived (thB) threads; the
	// ownership resolver must keep the archived one when explicitly a candidate.
	candidates := []ChannelInfo{
		{ChannelID: "grp1____thA", ChannelType: 5, ChannelName: "Active"},
		{ChannelID: "grp1____thB", ChannelType: 5, ChannelName: "Archived"},
		{ChannelID: "grp1____thC", ChannelType: 5, ChannelName: "Deleted"},
	}
	result := filterByOwnership(context.Background(), candidates, "user1", []string{"creator"}, db)
	got := threadIDSet(result)
	if !got["grp1____thA"] {
		t.Errorf("creator active thread should be retained, got %v", got)
	}
	if !got["grp1____thB"] {
		t.Errorf("creator archived thread should be retained, got %v", got)
	}
	if got["grp1____thC"] {
		t.Errorf("deleted thread must be excluded by ownership query, got %v", got)
	}
}

func TestFilterByOwnership_ArchivedThreadMemberRetained(t *testing.T) {
	db := setupPipelineImDB(t)
	seedPipelineThreads(db, time.Now().Unix())
	// Add a separate creator so user1 is a non-creator member of the threads.
	db.Exec(`UPDATE thread SET creator_uid = 'owner_other' WHERE id IN (1,2,3)`)

	candidates := []ChannelInfo{
		{ChannelID: "grp1____thA", ChannelType: 5, ChannelName: "Active"},
		{ChannelID: "grp1____thB", ChannelType: 5, ChannelName: "Archived"},
		{ChannelID: "grp1____thC", ChannelType: 5, ChannelName: "Deleted"},
	}
	result := filterByOwnership(context.Background(), candidates, "user1", []string{"member"}, db)
	got := threadIDSet(result)
	if !got["grp1____thB"] {
		t.Errorf("member archived thread should be retained, got %v", got)
	}
	if got["grp1____thC"] {
		t.Errorf("deleted thread must be excluded by member ownership query, got %v", got)
	}
}

// seedSharedArchivedThread seeds a group with both user1 and user2 as members of
// an archived thread that carries one message from each, so a multi-person
// summary (creator user1, participant user2) has mutual activity to retain.
func seedSharedArchivedThread(db *gorm.DB, ts int64) {
	db.Exec(`INSERT INTO "group" (group_no, name, space_id, status, creator) VALUES ('grp1', 'TestGroup', 'space1', 1, 'user1')`)
	db.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count, creator_uid) VALUES (1, 'thA', 'Active', 'grp1', 1, 5, 'user1')`)
	db.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count, creator_uid) VALUES (2, 'thB', 'Archived', 'grp1', 2, 4, 'user1')`)
	// Both members belong to both threads.
	db.Exec(`INSERT INTO thread_member (thread_id, uid) VALUES (1, 'user1'), (1, 'user2'), (2, 'user1'), (2, 'user2')`)
	db.Exec(`INSERT INTO group_member (group_no, uid, is_deleted, role) VALUES ('grp1', 'user1', 0, 0), ('grp1', 'user2', 0, 0)`)

	payload := `{"type":1,"content":"hello"}`
	db.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		1, "user1", "grp1____thB", 5, ts, []byte(payload), 0)
	db.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		2, "user2", "grp1____thB", 5, ts, []byte(payload), 0)
}

// TestResolveAndFetch_MultiPerson_SelectedArchivedThread_Retained is the
// regression guard for the Layer 1.5 blocker: a participant channel lookup that
// ignored the selected-thread scope removed the creator-side archived thread in
// the intersect, yielding an empty summary. With the scope threaded through, the
// shared archived thread survives and its messages are returned.
func TestResolveAndFetch_MultiPerson_SelectedArchivedThread_Retained(t *testing.T) {
	db := setupPipelineImDB(t)
	ts := time.Now().Add(-time.Hour).Unix()
	seedSharedArchivedThread(db, ts)

	specifiedSources := []map[string]interface{}{
		{"source_id": "grp1____thB", "source_type": 2}, // frontend thread type
	}

	start := time.Now().Add(-24 * time.Hour)
	end := time.Now().Add(time.Hour)

	msgs, err := ResolveAndFetchMessagesForPersonal(
		context.Background(), "user1", []string{"user2"}, nil, specifiedSources, "",
		start, end, db, nil, nil, 1, 0, 2, nil,
	)
	if err != nil {
		t.Fatalf("ResolveAndFetchMessagesForPersonal: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatalf("expected non-empty multi-person summary for selected archived thread, got 0 messages")
	}
	for _, m := range msgs {
		if m.ChannelID != "grp1____thB" {
			t.Errorf("unexpected message from %s, want only grp1____thB", m.ChannelID)
		}
	}
}

// TestIntersectParticipantChannels_EmptySelection_NoArchived asserts that an
// empty selected-thread slice keeps participant discovery on the status=1-only
// query, so an archived thread shared by both members is not surfaced.
func TestIntersectParticipantChannels_EmptySelection_NoArchived(t *testing.T) {
	db := setupPipelineImDB(t)
	seedSharedArchivedThread(db, time.Now().Unix())

	// Creator can see the archived thread (it was explicitly selected for them),
	// but with no selection threaded into the participant lookup, the intersect
	// drops it because user2's status=1-only discovery never includes it.
	creatorChannels, err := GetUserChannels(context.Background(), "user1", db, "grp1____thB")
	if err != nil {
		t.Fatalf("GetUserChannels: %v", err)
	}
	result, err := IntersectParticipantChannels(context.Background(), creatorChannels, []string{"user2"}, db)
	if err != nil {
		t.Fatalf("IntersectParticipantChannels: %v", err)
	}
	if threadIDSet(result)["grp1____thB"] {
		t.Errorf("empty selection must keep participant discovery on status=1 (no archived), got %v", threadIDSet(result))
	}
}
