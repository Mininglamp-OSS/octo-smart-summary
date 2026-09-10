//go:build cgo

package service

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"testing"
)

func TestGenerationConfigurationSQLite(t *testing.T) {
	db := contentTestDB(t)
	requireWriteOK(t, db.AutoMigrate(&model.SummarySource{}))
	exerciseGenerationConfiguration(t, db)
}

func TestGenerationScheduleSQLite(t *testing.T) {
	db := contentTestDB(t)
	requireWriteOK(t, db.AutoMigrate(&model.SummarySource{}))
	exerciseGenerationSchedule(t, db)
}
