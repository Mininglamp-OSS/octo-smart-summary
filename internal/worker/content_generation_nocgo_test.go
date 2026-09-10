//go:build !cgo

package worker

import (
	"testing"

	"gorm.io/gorm"
)

func contentWorkerSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	t.Skip("set SUMMARY_CONTENT_MYSQL_TEST_DSN or enable CGO for SQLite")
	return nil
}
