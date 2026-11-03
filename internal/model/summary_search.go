package model

import (
	"fmt"
	"reflect"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/width"
	"gorm.io/gorm"
)

func NormalizeSummarySearch(s string) string {
	return cases.Fold().String(width.Fold.String(s))
}

// All task creation paths (including scheduler and agent saves) persist search
// metadata in the same INSERT. TEXT accommodates Unicode case-fold expansions.
func (t *SummaryTask) BeforeCreate(tx *gorm.DB) error {
	t.SearchTitle = NormalizeSummarySearch(t.Title)
	t.SearchTopic = NormalizeSummarySearchTopic(t.Topic)
	return nil
}

func NormalizeSummarySearchTopic(s string) string {
	if strings.TrimSpace(s) == "" {
		return "" // DisplayTopic falls back to title, already searched separately.
	}
	return NormalizeSummarySearch(s)
}

// Map updates are used by regeneration, generation config and scheduler paths.
// Read the new values from Statement.Dest, not a possibly stale model instance.
func (t *SummaryTask) BeforeUpdate(tx *gorm.DB) error {
	selected, restricted := tx.Statement.SelectAndOmitColumns(false, true)
	for _, pair := range [][2]string{{"title", "SearchTitle"}, {"topic", "SearchTopic"}} {
		if allowed, exists := selected[pair[0]]; (exists && !allowed) || (!exists && restricted) {
			continue
		}
		var value string
		var present bool
		if updates, ok := tx.Statement.Dest.(map[string]interface{}); ok {
			v, exists := updates[pair[0]]
			if !exists {
				v, exists = updates[tx.Statement.Schema.LookUpField(pair[0]).Name]
			}
			if !exists {
				continue
			}
			value, present = v.(string)
			if !present {
				return fmt.Errorf("summary %s update requires a literal string", pair[0])
			}
		} else {
			v := reflect.Indirect(reflect.ValueOf(tx.Statement.Dest))
			if v.Kind() != reflect.Struct {
				continue
			}
			field := tx.Statement.Schema.LookUpField(pair[0])
			raw, zero := field.ValueOf(tx.Statement.Context, v)
			value, present = raw.(string)
			present = present && (!zero || selected[pair[0]])
		}
		if !present {
			continue
		}
		if pair[0] == "topic" {
			value = NormalizeSummarySearchTopic(value)
		} else {
			value = NormalizeSummarySearch(value)
		}
		tx.Statement.SetColumn(pair[1], value)
		if len(tx.Statement.Selects) > 0 {
			tx.Statement.Selects = append(tx.Statement.Selects, pair[1])
		}
	}
	return nil
}
