package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	summarydb "github.com/Mininglamp-OSS/octo-smart-summary/internal/db"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	migrationsql "github.com/Mininglamp-OSS/octo-smart-summary/migrations/sql"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	migrate "github.com/rubenv/sql-migrate"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests only accept a named disposable database and create separate,
// uniquely named databases. They never migrate or delete mirror/production data.
// Retained test DBs are intentional so failures can be inspected in the container.
func contentMySQLDB(t *testing.T, legacy bool) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("SUMMARY_CONTENT_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUMMARY_CONTENT_MYSQL_TEST_DSN is not set")
	}
	config, err := driver.ParseDSN(dsn)
	if err != nil || config.DBName != "summary_versioning_test" {
		t.Fatal("test DSN must select the disposable summary_versioning_test database")
	}
	admin, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "summary_versioning_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal(err)
	}
	config.DBName, config.ParseTime = name, true
	db, err := gorm.Open(mysql.Open(config.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(16)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if legacy {
		source := &migrate.EmbedFileSystemMigrationSource{FileSystem: migrationsql.FS, Root: "."}
		all, err := source.FindMigrations()
		if err != nil {
			t.Fatal(err)
		}
		var old []*migrate.Migration
		for _, migration := range all {
			if migration.Id < "20260908" {
				old = append(old, migration)
			}
		}
		if _, err := migrate.Exec(sqlDB, "mysql", &migrate.MemoryMigrationSource{Migrations: old}, migrate.Up); err != nil {
			t.Fatal(err)
		}
	} else if _, err := summarydb.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	t.Logf("retained disposable DB: %s", name)
	return db
}

func TestContentMySQLLegacyUpgradeAndReadOnlyProjection(t *testing.T) {
	db := contentMySQLDB(t, true)
	for _, statement := range []string{
		`INSERT INTO summary_task (id, task_no, space_id, creator_id, summary_mode, time_range_start, time_range_end, status, trigger_type)
		 VALUES (1, 'legacy-team', 's', 'owner', 2, NOW(), NOW(), 3, 2),
		        (2, 'legacy-personal', 's', 'owner', 2, NOW(), NOW(), 3, 3)`,
		`INSERT INTO summary_participant (id, task_id, user_id, status) VALUES (1, 1, 'member', 5), (2, 2, 'owner', 5)`,
		`INSERT INTO summary_result (id, task_id, content, version, generated_at) VALUES
		 (1, 1, 'v1', 1, NOW()), (2, 1, 'v7', 7, NOW())`,
		`UPDATE summary_task SET current_result_id = 2 WHERE id = 1`,
		`INSERT INTO summary_personal_result (task_id, participant_ref_id, user_id, content, created_at, updated_at)
		 VALUES (2, 2, 'owner', 'legacy body', NOW(), NOW())`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	sqlDB, _ := db.DB()
	n, err := summarydb.RunMigrations(sqlDB)
	if err != nil || n != 3 {
		t.Fatalf("upgrade migrations=%d: %v", n, err)
	}
	if n, err := summarydb.RunMigrations(sqlDB); err != nil || n != 0 {
		t.Fatalf("migration replay=%d: %v", n, err)
	}
	service := NewContentService(db)
	catalog, err := service.Catalog(context.Background(), "s", 1, "owner")
	if err != nil {
		t.Fatal(err)
	}
	current := catalog.Contents[0]
	if catalog.CreatedVia != "unknown" || current.ContentRevision != 2 ||
		current.CurrentVersion.Version != 7 || current.CurrentVersion.Content != "v7" {
		t.Fatalf("migration changed legacy meaning: %+v", catalog)
	}
	personal, err := service.Catalog(context.Background(), "s", 2, "owner")
	if err != nil || !personal.Contents[0].CurrentVersion.Provisional || personal.Contents[0].ContentRevision != 1 {
		t.Fatalf("personal compatibility: %+v %v", personal, err)
	}
	var rows int64
	if err := db.Model(&model.PersonalResultVersion{}).Count(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("GET wrote history: rows=%d err=%v", rows, err)
	}
}

func mysqlGenerationFixture(id, idempotency, slot string) model.SummaryGenerationRun {
	now := time.Now().UTC()
	return model.SummaryGenerationRun{
		ID: id, SpaceID: "s", TaskID: 1, ContentID: (ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentResult}).ID(),
		ActorID: "owner", OperationType: "refine", Executor: "refine", Scope: "content",
		IdempotencyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(idempotency))),
		RequestHash:     fmt.Sprintf("%x", sha256.Sum256([]byte("request"))),
		ActiveSlot:      &slot, EffectiveAt: now, BaseVersionID: "version",
		BaseContentRevision: 1, InputJSON: model.JSON(`{"content":"frozen"}`),
		Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
}

// Schema-level concurrency proof only; runtime claim/CAS/lease tests must be
// added when the shared writer is integrated. SQLite cannot prove this index.
func TestContentMySQLUniqueAdmissionAcrossConnections(t *testing.T) {
	db := contentMySQLDB(t, false)
	slot := fmt.Sprintf("%x", sha256.Sum256([]byte("s/1/result")))
	var accepted atomic.Int32
	var unexpected atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			run := mysqlGenerationFixture(uuid.NewString(), fmt.Sprint(i), slot)
			err := db.Transaction(func(tx *gorm.DB) error { return tx.Create(&run).Error })
			if err == nil {
				accepted.Add(1)
			} else {
				var duplicate *driver.MySQLError
				if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
					unexpected.Add(1)
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if accepted.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("admission accepted=%d unexpected=%d", accepted.Load(), unexpected.Load())
	}
	var winner model.SummaryGenerationRun
	if err := db.Where("active_slot = ?", slot).First(&winner).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&winner).Updates(map[string]any{"status": "completed", "active_slot": nil}).Error; err != nil {
		t.Fatal(err)
	}
	next := mysqlGenerationFixture(uuid.NewString(), "next", slot)
	if err := db.Create(&next).Error; err != nil {
		t.Fatalf("terminal slot not reusable: %v", err)
	}
	replay := mysqlGenerationFixture(uuid.NewString(), "next", slot)
	replay.ActiveSlot = nil
	if err := db.Create(&replay).Error; err == nil {
		t.Fatal("duplicate idempotency key accepted")
	}
}

func TestContentMySQLOutputAndScheduleUniqueness(t *testing.T) {
	db := contentMySQLDB(t, false)
	generation := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	result := model.SummaryResult{TaskID: 1, Content: "once", Version: 1, GenerationID: &generation, GeneratedAt: now}
	if err := db.Create(&result).Error; err != nil {
		t.Fatal(err)
	}
	result.ID, result.Version = 0, 2
	if err := db.Create(&result).Error; err == nil {
		t.Fatal("one generation created two result versions")
	}
	scheduleID := int64(99)
	first := mysqlGenerationFixture(uuid.NewString(), "slot-one", "one")
	first.ScheduleID, first.ScheduledFor = &scheduleID, &now
	if err := db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	second := mysqlGenerationFixture(uuid.NewString(), "slot-two", "two")
	second.ScheduleID, second.ScheduledFor = &scheduleID, &now
	if err := db.Create(&second).Error; err == nil {
		t.Fatal("duplicate schedule slot admitted")
	}
	// Preserve legacy duplicates for repair instead of deleting/renumbering.
	for _, body := range []string{"original duplicate A", "original duplicate B"} {
		legacy := model.SummaryResult{TaskID: 2, Content: body, Version: 1, GeneratedAt: now}
		if err := db.Create(&legacy).Error; err != nil {
			t.Fatalf("compatibility schema rejected legacy rows prematurely: %v", err)
		}
	}
}
