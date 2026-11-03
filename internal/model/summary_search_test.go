//go:build cgo

package model

import (
	"fmt"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSummarySearchWritePaths(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SummaryTask{}); err != nil {
		t.Fatal(err)
	}
	check := func(id int64) {
		t.Helper()
		var got SummaryTask
		if err := db.First(&got, id).Error; err != nil {
			t.Fatal(err)
		}
		if got.SearchTitle != NormalizeSummarySearch(got.Title) || got.SearchTopic != NormalizeSummarySearchTopic(got.Topic) {
			t.Fatalf("stale search fields: %+v", got)
		}
	}
	for i, update := range []func(*gorm.DB, *SummaryTask) error{
		func(db *gorm.DB, task *SummaryTask) error {
			return db.Model(task).Update("title", "ＳＴＲＡＳＳＥ").Error
		},
		func(db *gorm.DB, task *SummaryTask) error {
			return db.Model(&SummaryTask{}).Where("id = ?", task.ID).Updates(map[string]interface{}{"title": "ＮＥＷ", "topic": "Straße"}).Error
		},
		func(db *gorm.DB, task *SummaryTask) error {
			return db.Model(task).Updates(map[string]interface{}{"Title": "ＮＥＷ", "Topic": " \\t "}).Error
		},
		func(db *gorm.DB, task *SummaryTask) error {
			return db.Model(task).Updates(SummaryTask{Title: "ＮＥＷ", Topic: "Straße"}).Error
		},
		func(db *gorm.DB, task *SummaryTask) error {
			return db.Model(task).Select("topic").Updates(SummaryTask{Title: "omitted", Topic: ""}).Error
		},
		func(db *gorm.DB, task *SummaryTask) error {
			return db.Model(task).Select("title").Updates(map[string]interface{}{"title": "ＮＥＷ", "topic": "omitted"}).Error
		},
		func(db *gorm.DB, task *SummaryTask) error {
			task.Title = "ＮＥＷ"
			task.Topic = ""
			return db.Save(task).Error
		},
		func(db *gorm.DB, task *SummaryTask) error { return db.Model(task).Update("status", 2).Error },
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			task := SummaryTask{TaskNo: fmt.Sprint(i), Title: "ＯＣＴＯ", Topic: "Initial"}
			if err := db.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			check(task.ID)
			if err := update(db, &task); err != nil {
				t.Fatal(err)
			}
			check(task.ID)
		})
	}
}
