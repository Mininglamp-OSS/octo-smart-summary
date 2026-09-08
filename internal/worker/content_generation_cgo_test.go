//go:build cgo

package worker

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type workerTestRun struct {
	model.SummaryGenerationRun
	EffectiveAt  time.Time  `gorm:"column:effective_at;type:datetime;not null"`
	ScheduledFor *time.Time `gorm:"column:scheduled_for;type:datetime"`
	LeaseUntil   *time.Time `gorm:"column:lease_until;type:datetime"`
	CreatedAt    time.Time  `gorm:"column:created_at;type:datetime;not null"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;type:datetime;not null"`
}

func (workerTestRun) TableName() string { return "summary_generation_run" }

type workerTestAudit struct {
	model.SummaryContentAudit
	CreatedAt time.Time `gorm:"column:created_at;type:datetime;not null"`
}

func (workerTestAudit) TableName() string { return "summary_content_audit" }

func contentWorkerSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	workerCheck(t, err)
	raw, err := db.DB()
	workerCheck(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { raw.Close() })
	workerCheck(t, db.AutoMigrate(&model.SummaryTask{}, &model.SummaryParticipant{}, &model.PersonalResult{},
		&model.PersonalResultVersion{}, &model.SummaryResult{}, &model.SummarySchedule{}, &workerTestRun{}, &workerTestAudit{}))
	return db
}
