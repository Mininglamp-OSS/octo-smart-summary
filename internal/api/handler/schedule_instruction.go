package handler

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxScheduleInstructionRunes = 8000

func normalizeScheduleGenerationInstruction(v string) (string, error) {
	out := strings.TrimSpace(v)
	if utf8.RuneCountInString(out) > maxScheduleInstructionRunes {
		return "", service.NewBizError(40010, "定时生成要求不能超过 8000 字符", http.StatusBadRequest)
	}
	return out, nil
}

func appendBoundScheduleGenerationInstruction(tx *gorm.DB, task model.SummaryTask, feedback string) error {
	if task.ScheduleID == nil {
		return nil
	}
	addition := strings.TrimSpace(feedback)
	if addition == "" {
		return nil
	}
	var sched model.SummarySchedule
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", *task.ScheduleID, task.SpaceID).
		First(&sched).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	current := strings.TrimSpace(sched.GenerationInstruction)
	next := addition
	if current != "" {
		next = current + "\n" + addition
	}
	next = keepLatestRunes(next, maxScheduleInstructionRunes)
	return tx.Model(&model.SummarySchedule{}).
		Where("id = ?", sched.ID).
		Update("generation_instruction", next).Error
}

func keepLatestRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return strings.TrimSpace(string(runes[len(runes)-maxRunes:]))
}

func resetBoundScheduleGenerationInstruction(tx *gorm.DB, task model.SummaryTask, instruction string) error {
	if task.ScheduleID == nil {
		return nil
	}
	next, err := normalizeScheduleGenerationInstruction(instruction)
	if err != nil {
		return err
	}
	return tx.Model(&model.SummarySchedule{}).
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", *task.ScheduleID, task.SpaceID).
		Update("generation_instruction", next).Error
}
