package handler

// snapshot_scope_input.go — SUM-BE1 helper glue.
//
// The shared service.ValidateSnapshotScope validator takes its own
// SourceInput / ParticipantInput / ScheduleInput types so it can live in
// internal/service/ without a cycle back into internal/api/handler/. This
// file provides the tiny converters + one init() that wires the runtime
// pipeline.DefaultTimeRangeDays global into the validator's getter, so a
// single boot-time change to DEFAULT_TIME_RANGE_DAYS still bounds new
// summaries the same way the old inline check did.

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func init() {
	// Wire the pipeline runtime global into the shared validator so a
	// config-driven DEFAULT_TIME_RANGE_DAYS change is honoured on every
	// request without either package importing the other for a value.
	service.SetTimeRangeMaxDaysGetter(func() int { return pipeline.DefaultTimeRangeDays })
}

// snapshotSourcesFromReq converts the handler-side sourceReq slice into the
// validator's SourceInput slice. Nil in -> nil out so the validator's empty
// / count-cap checks stay literal.
func snapshotSourcesFromReq(in []sourceReq) []service.SourceInput {
	if len(in) == 0 {
		return nil
	}
	out := make([]service.SourceInput, len(in))
	for i, s := range in {
		out[i] = service.SourceInput{SourceType: s.SourceType, SourceID: s.SourceID}
	}
	return out
}

// snapshotParticipantsFromReq mirrors snapshotSourcesFromReq for participants.
// Empty in -> nil out so validators keying off len(...) == 0 continue to
// behave as they did on the pre-refactor req.Participants slice.
func snapshotParticipantsFromReq(in []participantReq) []service.ParticipantInput {
	if len(in) == 0 {
		return nil
	}
	out := make([]service.ParticipantInput, len(in))
	for i, p := range in {
		out[i] = service.ParticipantInput{UserID: p.UserID, UserName: p.UserName}
	}
	return out
}

// snapshotScheduleFromReq converts createScheduleReq's schedule fields into
// the validator's ScheduleInput. It never nil-returns because the CreateSchedule
// handler always has a full schedule shape at hand.
func snapshotScheduleFromReq(req createScheduleReq) *service.ScheduleInput {
	return &service.ScheduleInput{
		CronExpr:       req.CronExpr,
		IntervalDays:   req.IntervalDays,
		IntervalMonths: req.IntervalMonths,
		RunTime:        req.RunTime,
		DayOfWeek:      req.DayOfWeek,
		DayOfMonth:     req.DayOfMonth,
		TimeRangeType:  req.TimeRangeType,
	}
}
