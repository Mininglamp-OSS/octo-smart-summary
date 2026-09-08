package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	summarydb "github.com/Mininglamp-OSS/octo-smart-summary/internal/db"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func contentWorkerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("SUMMARY_CONTENT_MYSQL_TEST_DSN")
	if dsn == "" {
		return contentWorkerSQLiteDB(t)
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil || cfg.DBName != "summary_versioning_test" {
		t.Fatal("test DSN must select disposable summary_versioning_test")
	}
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	workerCheck(t, err)
	defer admin.Close()
	name := "summary_versioning_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
	workerCheck(t, err)
	cfg.DBName, cfg.ParseTime = name, true
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	workerCheck(t, err)
	raw, err := db.DB()
	workerCheck(t, err)
	t.Cleanup(func() { raw.Close() })
	_, err = summarydb.RunMigrations(raw)
	workerCheck(t, err)
	t.Logf("retained disposable DB: %s", name)
	return db
}

type contentWorkerModel func(context.Context, []service.ChatMessage, float64) (string, int, string, error)

func (f contentWorkerModel) CallWithModel(ctx context.Context, messages []service.ChatMessage, temp float64) (string, int, string, error) {
	return f(ctx, messages, temp)
}

func workerCheck(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func seedWorkerRefine(t *testing.T, db *gorm.DB) *model.SummaryGenerationRun {
	t.Helper()
	now := time.Now()
	task := model.SummaryTask{TaskNo: service.GenerateTaskNo(), SpaceID: "s", CreatorID: "owner",
		SummaryMode: model.ModeByPerson, Status: model.StatusCompleted, TimeRangeStart: now, TimeRangeEnd: now}
	workerCheck(t, db.Create(&task).Error)
	member := model.SummaryParticipant{TaskID: task.ID, UserID: "owner", Status: model.ParticipantSubmitted}
	workerCheck(t, db.Create(&member).Error)
	workerCheck(t, db.Create(&model.PersonalResult{TaskID: task.ID, ParticipantRefID: member.ID, UserID: "owner",
		Content: "original [7]", CitationsJSON: `[{"index":7,"content":"frozen evidence"}]`,
		WorkerStatus: model.PersonalStatusCompleted, GeneratedAt: &now}).Error)
	s := service.NewContentService(db)
	catalog, err := s.Catalog(context.Background(), "s", task.ID, "owner")
	workerCheck(t, err)
	current := catalog.Contents[0].CurrentVersion
	run, err := s.QueueRefine(context.Background(), "s", task.ID, "owner", catalog.MainContentID, service.RefineContentRequest{
		ContentBaseline: service.ContentBaseline{ExpectedCurrentVersionID: current.VersionID, ExpectedContentRevision: current.ContentRevision},
		IdempotencyKey:  "first", Feedback: "shorter",
	})
	workerCheck(t, err)
	return run
}

func TestContentWorkerScansCommittedQueueAndRespectsWriteAllowlist(t *testing.T) {
	db := contentWorkerTestDB(t)
	run := seedWorkerRefine(t, db)
	pool := NewWorkerPool(1)
	var calls atomic.Int32
	w := NewContentGenerationWorker(db, pool, contentWorkerModel(func(_ context.Context, messages []service.ChatMessage, _ float64) (string, int, string, error) {
		calls.Add(1)
		if !strings.Contains(messages[1].Content, "original [7]") || !strings.Contains(messages[1].Content, "frozen evidence") {
			return "", 0, "", errors.New("missing frozen evidence")
		}
		return "shorter [7]", 1, "fixture", nil
	}), time.Second)
	t.Setenv("SUMMARY_CONTENT_READ_SPACES", "s")
	for _, spaces := range []string{"", "*", "S"} {
		t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", spaces)
		n, err := w.poll(context.Background())
		workerCheck(t, err)
		if n != 0 || calls.Load() != 0 {
			t.Fatalf("non-enrolled Space executed: %q", spaces)
		}
	}
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "s")
	n, err := w.poll(context.Background())
	workerCheck(t, err)
	if n != 1 {
		t.Fatalf("did not discover queued run: %d", n)
	}
	pool.Drain()
	var result model.SummaryGenerationRun
	workerCheck(t, db.First(&result, "id = ?", run.ID).Error)
	if result.Status != "completed" || !result.Applied || result.ExecutionToken != 1 {
		t.Fatalf("run not committed: %+v", result)
	}
	n, err = w.poll(context.Background())
	workerCheck(t, err)
	if n != 0 || calls.Load() != 1 {
		t.Fatal("duplicate scan repeated model execution")
	}
	var count int64
	workerCheck(t, db.Model(&model.PersonalResultVersion{}).Where("generation_id = ?", run.ID).Count(&count).Error)
	if count != 1 {
		t.Fatalf("expected one output, got %d", count)
	}
}

func TestContentWorkerRestartRecoversInterruptedLeaseWithoutHTTPWakeup(t *testing.T) {
	db := contentWorkerTestDB(t)
	run := seedWorkerRefine(t, db)
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "s")
	pool := NewWorkerPool(1)
	started := make(chan struct{})
	w := NewContentGenerationWorker(db, pool, contentWorkerModel(func(ctx context.Context, _ []service.ChatMessage, _ float64) (string, int, string, error) {
		close(started)
		<-ctx.Done()
		return "", 0, "", ctx.Err()
	}), time.Hour)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("startup scan did not execute durable run")
	}
	stop()
	<-done
	pool.Drain()
	var interrupted model.SummaryGenerationRun
	workerCheck(t, db.First(&interrupted, "id = ?", run.ID).Error)
	if interrupted.Status != "running" || interrupted.ActiveSlot == nil || interrupted.OutputVersionID != "" {
		t.Fatal("shutdown incorrectly terminalized the recoverable run")
	}
	// Simulate the passage of the lease interval without sleeping two minutes.
	workerCheck(t, db.Model(&model.SummaryGenerationRun{}).Where("id = ?", run.ID).
		Update("lease_until", time.Now().Add(-time.Minute)).Error)
	restarted := NewContentGenerationWorker(db, pool, contentWorkerModel(func(context.Context, []service.ChatMessage, float64) (string, int, string, error) {
		return "recovered [7]", 1, "fixture", nil
	}), time.Hour)
	n, err := restarted.poll(context.Background())
	workerCheck(t, err)
	if n != 1 {
		t.Fatal("expired lease not rediscovered")
	}
	pool.Drain()
	var completed model.SummaryGenerationRun
	workerCheck(t, db.First(&completed, "id = ?", run.ID).Error)
	if completed.Status != "completed" || completed.ExecutionToken != 2 || !completed.Applied {
		t.Fatalf("restart failed: %+v", completed)
	}
}

func TestContentWorkerSaturationLeavesQueueRecoverable(t *testing.T) {
	db := contentWorkerTestDB(t)
	seedWorkerRefine(t, db)
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "s")
	pool := NewWorkerPool(1)
	block := make(chan struct{})
	pool.Submit(func() { <-block })
	w := NewContentGenerationWorker(db, pool, contentWorkerModel(func(context.Context, []service.ChatMessage, float64) (string, int, string, error) {
		return "done [7]", 1, "fixture", nil
	}), time.Second)
	n, err := w.poll(context.Background())
	close(block)
	pool.Drain()
	workerCheck(t, err)
	if n != 0 {
		t.Fatal("saturated pool admitted work")
	}
	n, err = w.poll(context.Background())
	workerCheck(t, err)
	pool.Drain()
	if n != 1 {
		t.Fatal("local in-flight marker stranded pending work")
	}
}

func TestContentWorkerLegacyFailureCASMySQL(t *testing.T) {
	if os.Getenv("SUMMARY_CONTENT_MYSQL_TEST_DSN") == "" {
		t.Skip("requires real MySQL concurrent connections")
	}
	db := contentWorkerTestDB(t)
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "")
	now := time.Now()
	task := model.SummaryTask{TaskNo: service.GenerateTaskNo(), SpaceID: "s", CreatorID: "owner",
		SummaryMode: model.ModeByPerson, Status: model.StatusProcessing, TimeRangeStart: now, TimeRangeEnd: now}
	workerCheck(t, db.Create(&task).Error)
	member := model.SummaryParticipant{TaskID: task.ID, UserID: "owner", Status: model.ParticipantProcessing}
	workerCheck(t, db.Create(&member).Error)
	workerCheck(t, db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "other", Status: model.ParticipantAccepted}).Error)
	pr := model.PersonalResult{TaskID: task.ID, ParticipantRefID: member.ID, UserID: "owner",
		WorkerStatus: model.PersonalStatusProcessing}
	workerCheck(t, db.Create(&pr).Error)
	p := &Processor{db: db, cfg: &config.Config{WorkerMaxRetry: 1000}}
	const attempts = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ownPR, ownMember := pr, member
			p.markPersonalFailed(&ownPR, &ownMember, "synthetic failure")
		}()
	}
	close(start)
	wg.Wait()
	var saved model.PersonalResult
	workerCheck(t, db.First(&saved, pr.ID).Error)
	if saved.RetryCount != attempts {
		t.Fatalf("legacy task fence lost retry increments: %d", saved.RetryCount)
	}
}
