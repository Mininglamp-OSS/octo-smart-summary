package worker

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func (p *Processor) scheduleGenerationInstruction(task model.SummaryTask) string {
	if task.TriggerType != model.TriggerScheduled || task.ScheduleID == nil {
		return ""
	}
	var sched model.SummarySchedule
	if err := p.db.Select("generation_instruction").
		Where("id = ? AND deleted_at IS NULL", *task.ScheduleID).
		First(&sched).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(sched.GenerationInstruction)
}

func (p *Processor) generationTopic(task model.SummaryTask) string {
	topic := task.EffectiveTopic()
	instruction := p.scheduleGenerationInstruction(task)
	if instruction == "" {
		return topic
	}
	if strings.TrimSpace(topic) == "" {
		return instruction
	}
	return fmt.Sprintf("%s\n\n定时生成要求：\n%s", topic, instruction)
}

func (p *Processor) scheduledOperationNote(task model.SummaryTask) string {
	instruction := p.scheduleGenerationInstruction(task)
	if instruction != "" {
		return instruction
	}
	return task.Title
}
