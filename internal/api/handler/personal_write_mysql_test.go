//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type personalWriteProbeKey struct{}

// Opt-in against a disposable database only; unlike SQLite this exercises real
// FOR UPDATE waits and current reads at MySQL's default REPEATABLE READ level.
func TestPersonalWritesMySQLConcurrency(t *testing.T) {
	dsn := os.Getenv("SUMMARY_CR_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUMMARY_CR_MYSQL_TEST_DSN is not set")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal("cannot open isolated MySQL")
	}
	var database string
	if err := db.Raw("SELECT DATABASE()").Scan(&database).Error; err != nil || !strings.HasPrefix(database, "summary_cr_") {
		t.Fatal("requires a disposable summary_cr_* database")
	}
	for _, table := range []interface{}{&model.SummaryTask{}, &model.SummaryParticipant{}, &model.PersonalResult{},
		&model.PersonalResultVersion{}, &model.SummaryResult{}, &model.SummarySchedule{}} {
		if !db.Migrator().HasTable(table) {
			if err := db.Migrator().CreateTable(table); err != nil {
				t.Fatal(err)
			}
		}
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	// Both refinement transports produce identical synthetic content; no real
	// model or production endpoint is contacted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"refined [1]\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"refined [1]"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	for _, operation := range []string{"edit", "refine", "stream", "restore"} {
		for _, change := range []string{"task-status", "display-target", "personal-version"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				now := time.Now().UTC()
				task := model.SummaryTask{TaskNo: fmt.Sprintf("CR-%d", now.UnixNano()), SpaceID: "space1",
					CreatorID: "creator1", SummaryMode: model.ModeByPerson, Status: model.StatusCompleted,
					TimeRangeStart: now.Add(-time.Hour), TimeRangeEnd: now}
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				must(db.Create(&task).Error)
				participant := model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", Status: model.ParticipantCompleted}
				must(db.Create(&participant).Error)
				pr := model.PersonalResult{TaskID: task.ID, ParticipantRefID: participant.ID, UserID: "creator1",
					WorkerStatus: model.PersonalStatusCompleted, Content: "old [1]", CitationsJSON: `[{"index":1}]`, GeneratedAt: &now}
				must(db.Create(&pr).Error)
				v1 := model.PersonalResultVersion{TaskID: task.ID, ParticipantRefID: participant.ID, UserID: "creator1",
					Version: 1, Content: pr.Content, CitationsJSON: pr.CitationsJSON, GeneratedAt: now}
				must(db.Create(&v1).Error)
				v2 := v1
				v2.ID, v2.Version, v2.Content = 0, 2, "concurrent version [1]"
				must(db.Create(&v2).Error)
				must(db.Model(&pr).Update("current_version_id", v1.ID).Error)
				r1 := model.SummaryResult{TaskID: task.ID, Version: 1, Content: "old display", GeneratedAt: now}
				r2 := model.SummaryResult{TaskID: task.ID, Version: 2, Content: "new display", GeneratedAt: now}
				must(db.Create(&r1).Error)
				must(db.Create(&r2).Error)
				must(db.Model(&task).Update("current_result_id", r1.ID).Error)

				firstLock := make(chan string, 1)
				var once sync.Once
				callback := "cr_personal_lock_order"
				must(db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Context.Value(personalWriteProbeKey{}) == true {
						if _, locked := tx.Statement.Clauses["FOR"]; locked {
							once.Do(func() { firstLock <- tx.Statement.Table })
						}
					}
				}))
				defer db.Callback().Query().Remove(callback)
				h := NewPersonalHandler(db.WithContext(context.WithValue(context.Background(), personalWriteProbeKey{}, true)), "", nil)
				h.SetLLM(service.NewLLMClient(srv.URL, "test", "test", 5, 256, false, 5, nil))
				router := gin.New()
				router.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
				method, path, body := "POST", "", `{"feedback":"refine","base_version":1}`
				switch operation {
				case "edit":
					method, path, body = "PUT", "personal-edit", `{"content":"edited [1]"}`
					router.PUT("/api/v1/summaries/:id/"+path, h.PersonalEdit)
				case "refine":
					path = "personal-refine"
					router.POST("/api/v1/summaries/:id/"+path, h.RefinePersonalSummary)
				case "stream":
					path = "personal-refine/stream"
					router.POST("/api/v1/summaries/:id/"+path, h.RefinePersonalSummaryStream)
				case "restore":
					path = fmt.Sprintf("personal-versions/%d/restore", v1.ID)
					router.POST("/api/v1/summaries/:id/personal-versions/:version_id/restore", h.RestorePersonalVersion)
				}
				blocker := db.Begin()
				defer blocker.Rollback()
				must(blocker.Clauses(clause.Locking{Strength: "UPDATE"}).First(&model.SummaryTask{}, task.ID).Error)
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					req := httptest.NewRequest(method, fmt.Sprintf("/api/v1/summaries/%d/%s", task.ID, path), strings.NewReader(body))
					req.Header.Set("Token", "creator1")
					req.Header.Set("X-Space-Id", "space1")
					req.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					router.ServeHTTP(w, req)
					done <- w
				}()
				select {
				case table := <-firstLock:
					if table != "summary_task" {
						t.Errorf("first lock is %s, want summary_task (ABBA risk)", table)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("handler did not reach persistence")
				}
				// While the handler waits on task, its result must remain
				// lockable: an inverted personal-first writer fails NOWAIT.
				must(blocker.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).First(&model.PersonalResult{}, pr.ID).Error)
				switch change {
				case "task-status":
					must(blocker.Model(&task).Update("status", model.StatusPending).Error)
				case "display-target":
					must(blocker.Model(&task).Update("current_result_id", r2.ID).Error)
				case "personal-version":
					must(blocker.Model(&pr).Updates(map[string]interface{}{"content": v2.Content, "current_version_id": v2.ID}).Error)
				}
				must(blocker.Commit().Error)
				var response *httptest.ResponseRecorder
				select {
				case response = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("handler did not finish after releasing lock")
				}
				if change != "display-target" {
					if operation == "stream" {
						if !strings.Contains(response.Body.String(), "event: error") || strings.Contains(response.Body.String(), "event: done") {
							t.Fatalf("expected streamed conflict: %s", response.Body)
						}
					} else if response.Code != http.StatusConflict {
						t.Fatalf("expected409, got%d: %s", response.Code, response.Body)
					}
					must(db.First(&pr, pr.ID).Error)
					expected := v1.Content
					if change == "personal-version" {
						expected = v2.Content
					}
					if pr.Content != expected {
						t.Fatal("concurrent data overwritten")
					}
					return
				}
				if response.Code != http.StatusOK || (operation == "stream" && !strings.Contains(response.Body.String(), "event: done")) {
					t.Fatalf("write failed: %d %s", response.Code, response.Body)
				}
				must(db.First(&r1, r1.ID).Error)
				must(db.First(&r2, r2.ID).Error)
				expected := "refined [1]"
				if operation == "edit" {
					expected = "edited [1]"
				}
				if operation == "restore" {
					expected = v1.Content
				}
				if r1.Content != "old display" || r2.Content != expected {
					t.Fatalf("wrong display target: R1=%q R2=%q", r1.Content, r2.Content)
				}
			})
		}
	}
}
