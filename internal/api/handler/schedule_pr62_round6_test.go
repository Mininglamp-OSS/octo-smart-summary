package handler

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// PR#62 r6 tests: only the behaviors new this round (Lock-Order, post-load
// recheck, 1062->409). Round-5 bodies were renamed to TestPR62Round5_* so
// `go test -run TestPR62Round` selects both rounds.
//
// SQLite has no row-level FOR UPDATE, so the lock-order Blocker is verified by a
// static source-order assertion; the 1062 mapping is unit-tested on the mapping
// function and the switch wiring.

// Static proof: in UpdateSchedule and CreateSchedule, peekTaskScheduleID and the
// schedule lock run before loadTaskForTaskScope (the only task lock), so the tx
// is strictly schedule->task.
func TestPR62Round6_LockOrder_ScheduleBeforeTask(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "schedule.go", nil, 0)
	if err != nil {
		t.Fatalf("parse schedule.go: %v", err)
	}

	for _, fn := range []string{"UpdateSchedule", "CreateSchedule"} {
		decl := findFuncDecl(f, fn)
		if decl == nil {
			t.Fatalf("func %s not found", fn)
		}
		taskLockPos := -1
		lastScheduleLockPos := -1
		peekPos := -1
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch ident.Name {
			case "loadTaskForTaskScope":
				if taskLockPos == -1 || int(call.Pos()) < taskLockPos {
					taskLockPos = int(call.Pos())
				}
			case "lockScheduleForUpdate":
				if int(call.Pos()) > lastScheduleLockPos {
					lastScheduleLockPos = int(call.Pos())
				}
			case "peekTaskScheduleID":
				peekPos = int(call.Pos())
			}
			return true
		})

		if taskLockPos == -1 {
			t.Fatalf("%s: no loadTaskForTaskScope (task lock) call found", fn)
		}
		if peekPos == -1 || peekPos >= taskLockPos {
			t.Fatalf("%s: peekTaskScheduleID must run before loadTaskForTaskScope (peek=%d taskLock=%d)", fn, peekPos, taskLockPos)
		}
		// CreateSchedule has no lockScheduleForUpdate; assert only if present.
		if lastScheduleLockPos != -1 && lastScheduleLockPos >= taskLockPos {
			t.Fatalf("%s: lockScheduleForUpdate must run before loadTaskForTaskScope (schedLock=%d taskLock=%d)", fn, lastScheduleLockPos, taskLockPos)
		}
	}

	// UpdateSchedule must reuse the pre-locked oldSched.
	if !strings.Contains(readHandlerSource(t), "lockedOldSched") {
		t.Fatalf("expected UpdateSchedule to reuse a pre-locked oldSched (lockedOldSched)")
	}
}

func findFuncDecl(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

func readHandlerSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("schedule.go")
	if err != nil {
		t.Fatalf("read schedule.go: %v", err)
	}
	return string(b)
}

// Post-load recheck: req.Participants is re-validated against the loaded
// task.CreatorID, so a non-creator participant is rejected and the task stays
// unbound. (The creator-only pass case is covered by round-5 stored-config and
// clone bind tests.)
func TestPR62Round6_PostLoadRecheck_ReqParticipantsMultiPersonRejected(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "s", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "17:00", TimeRangeType: 2, IsActive: 1}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{TaskNo: "R6-POSTLOAD", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", Status: model.ParticipantAccepted})

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"scope":    "task",
		"task_id":  task.ID,
		"run_time": "09:30", "interval_days": 1,
		"participants": []map[string]string{
			{"user_id": "creator1", "user_name": "C"},
			{"user_id": "stranger", "user_name": "S"},
		},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for multi-person req participants, got %d body=%s", w.Code, w.Body.String())
	}
	if code := decodeCode(t, w.Body); code != codeTeamScheduleNotSupported {
		t.Fatalf("expected code %d, got %d body=%s", codeTeamScheduleNotSupported, code, w.Body.String())
	}
	var gotTask model.SummaryTask
	db.First(&gotTask, task.ID)
	if gotTask.ScheduleID != nil {
		t.Fatalf("task must remain unbound after rejection, got schedule_id=%v", *gotTask.ScheduleID)
	}
}

// Unit: the unified post-load validator covers both req and stored-config paths.
func TestPR62Round6_ValidateEffectiveParticipants_Unit(t *testing.T) {
	creator := "creator1"
	if err := validateEffectiveParticipantsSubsetOfCreator([]participantReq{{UserID: creator}}, nil, creator); err != nil {
		t.Fatalf("creator-only req should pass, got %v", err)
	}
	if err := validateEffectiveParticipantsSubsetOfCreator([]participantReq{{UserID: creator}, {UserID: "other"}}, nil, creator); !errors.Is(err, errMultiPersonNotSupported) {
		t.Fatalf("multi-person req should be rejected, got %v", err)
	}
	stored := mustParticipantConfig(t, [2]string{creator, "C"}, [2]string{"other", "O"})
	if err := validateEffectiveParticipantsSubsetOfCreator(nil, stored, creator); !errors.Is(err, errMultiPersonNotSupported) {
		t.Fatalf("multi-person stored config should be rejected, got %v", err)
	}
	if err := validateEffectiveParticipantsSubsetOfCreator(nil, nil, creator); err != nil {
		t.Fatalf("nil req + empty stored should pass, got %v", err)
	}
}

// Unit: MySQL 1062 detection (raw, wrapped, gorm sentinel; non-1062 and nil are
// not duplicate-key).
func TestPR62Round6_IsMySQLDuplicateKey_Unit(t *testing.T) {
	dup := &mysqldriver.MySQLError{Number: 1062, Message: "Duplicate entry"}
	if !isMySQLDuplicateKey(dup) {
		t.Fatalf("raw 1062 should be detected")
	}
	if !isMySQLDuplicateKey(&wrapErr{dup}) {
		t.Fatalf("wrapped 1062 should be detected via errors.As")
	}
	if !isMySQLDuplicateKey(gorm.ErrDuplicatedKey) {
		t.Fatalf("gorm.ErrDuplicatedKey should be detected")
	}
	if isMySQLDuplicateKey(&mysqldriver.MySQLError{Number: 1213, Message: "Deadlock"}) {
		t.Fatalf("non-1062 driver error must not be duplicate-key")
	}
	if isMySQLDuplicateKey(nil) || isMySQLDuplicateKey(errors.New("plain")) {
		t.Fatalf("nil/plain must not be duplicate-key")
	}
}

type wrapErr struct{ inner error }

func (w *wrapErr) Error() string { return "wrap: " + w.inner.Error() }
func (w *wrapErr) Unwrap() error { return w.inner }

// Wiring: errLiveBindingDuplicate is referenced in its def plus both bind
// switches and maps to 409.
func TestPR62Round6_DuplicateKey_MapsTo409Code(t *testing.T) {
	src := readHandlerSource(t)
	if strings.Count(src, "errLiveBindingDuplicate") < 3 {
		t.Fatalf("errLiveBindingDuplicate must appear in def + Create + Update switches")
	}
	if !strings.Contains(src, "http.StatusConflict") {
		t.Fatalf("expected duplicate-key mapping to http.StatusConflict (409)")
	}
}
