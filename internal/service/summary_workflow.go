package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// MaxSummaryWorkflowIdempotencyKeyLen matches the existing bot-create and
	// Agent-save retry contracts.
	MaxSummaryWorkflowIdempotencyKeyLen = 128

	// WorkflowIdempotencyMismatchCode identifies reuse of one key for two
	// semantically different workflow requests.
	WorkflowIdempotencyMismatchCode = 40009

	// AgentSummaryDefaultTimeRangeDays is the unified summary workspace fallback
	// whenever the user does not explicitly select a time range.
	AgentSummaryDefaultTimeRangeDays = 30

	// legacySummaryDefaultTimeRangeDays is used only when the caller does not
	// provide the configured legacy default.
	legacySummaryDefaultTimeRangeDays = 31
)

var (
	summaryWorkflowIdempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	errSummaryWorkflowIdempotencyRace    = errors.New("summary workflow idempotency race")
)

// SummaryWorkflowIdempotencyError carries the original task id so a client
// can recover from a mismatched retry or a key whose task was deleted.
type SummaryWorkflowIdempotencyError struct {
	ExistingTaskID int64
	Reason         string
	RecoveryAction string
	BizError       *BizError
}

func (e *SummaryWorkflowIdempotencyError) Error() string {
	return e.BizError.Error()
}

func (e *SummaryWorkflowIdempotencyError) Unwrap() error {
	return e.BizError
}

// SummaryWorkflowTarget is the deterministic execution path selected from a
// normalized workflow request.
type SummaryWorkflowTarget string

const (
	SummaryWorkflowPersonal SummaryWorkflowTarget = "personal_workflow"
	SummaryWorkflowTeam     SummaryWorkflowTarget = "team_workflow"
)

// SummaryWorkflowTimeRange is the service-layer form of the HTTP time range.
type SummaryWorkflowTimeRange struct {
	Start time.Time
	End   time.Time
}

// SummaryWorkflowSource is one chat/thread/direct-message input source.
type SummaryWorkflowSource struct {
	SourceType int
	SourceID   string
}

// SummaryWorkflowParticipant is one requested workflow participant. The
// creator is always materialized as Accepted even when omitted here.
type SummaryWorkflowParticipant struct {
	UserID   string
	UserName string
}

// LegacyCreateSummaryWorkflowInput is the compatibility command for the
// existing HTTP endpoint. CreatorID preserves its historical uid override;
// this type must never be exposed as an Agent tool schema.
type LegacyCreateSummaryWorkflowInput struct {
	ActorID             string
	CreatorID           string
	SpaceID             string
	Title               string
	Topic               string
	TimeRange           *SummaryWorkflowTimeRange
	Sources             []SummaryWorkflowSource
	Participants        []SummaryWorkflowParticipant
	ConfirmTimeoutHours int
	OriginChannelID     string
	OriginChannelType   int
	IdempotencyKey      string
}

// AgentCreateSummaryWorkflowInput is the trusted application command used by
// the unified summary workspace. Unlike LegacyCreateSummaryWorkflowInput it
// has no creator override: ActorID is always the task creator. Callers must
// already have resolved and authorised the source/participant identities.
type AgentCreateSummaryWorkflowInput struct {
	ActorID             string
	SpaceID             string
	Title               string
	Requirement         string
	TimeRange           *SummaryWorkflowTimeRange
	Sources             []SummaryWorkflowSource
	Participants        []SummaryWorkflowParticipant
	ConfirmTimeoutHours int
	OriginChannelID     string
	OriginChannelType   int
	IdempotencyKey      string
	// AgentSessionID marks asynchronous workflows created by the unified Agent
	// workspace. The worker uses it to preserve the workspace's documented
	// participant-union source semantics without changing legacy workflows.
	AgentSessionID string
}

// CreateSummaryWorkflowResult contains the committed task and any worker
// dispatch needed after commit. A replay may return a trigger when its worker
// row is still pending so the caller can recover a previously lost dispatch.
type CreateSummaryWorkflowResult struct {
	Task                 model.SummaryTask
	CreatorParticipantID int64
	Target               SummaryWorkflowTarget
	Inferred             bool
	Replayed             bool
	WorkerTrigger        *model.WorkerTriggerRequest
}

// SummaryWorkflowService owns the task/source/participant transaction that the
// traditional endpoint uses now and policy-gated Agent adapters can reuse
// later.
type SummaryWorkflowService struct {
	db                   *gorm.DB
	imDB                 *gorm.DB
	defaultTimeRangeDays int
	maxTimeRangeDays     int
}

// NewSummaryWorkflowService creates the application service. maxTimeRangeDays
// is supplied by the caller to avoid a service -> pipeline import cycle.
func NewSummaryWorkflowService(db, imDB *gorm.DB, defaultTimeRangeDays, maxTimeRangeDays int) *SummaryWorkflowService {
	if defaultTimeRangeDays <= 0 {
		defaultTimeRangeDays = legacySummaryDefaultTimeRangeDays
	}
	return &SummaryWorkflowService{
		db:                   db,
		imDB:                 imDB,
		defaultTimeRangeDays: defaultTimeRangeDays,
		maxTimeRangeDays:     maxTimeRangeDays,
	}
}

// ValidSummaryWorkflowIdempotencyKey reports whether a non-empty key is safe
// for persistence. The HTTP endpoint keeps the header optional for backward
// compatibility; future Agent entry points must require it.
func ValidSummaryWorkflowIdempotencyKey(key string) bool {
	return len(key) > 0 &&
		len(key) <= MaxSummaryWorkflowIdempotencyKeyLen &&
		summaryWorkflowIdempotencyKeyPattern.MatchString(key)
}

type normalizedSummaryWorkflowInput struct {
	actorID             string
	creatorID           string
	spaceID             string
	title               string
	topic               string
	timeStart           time.Time
	timeEnd             time.Time
	explicitTimeRange   bool
	sources             []SummaryWorkflowSource
	participants        []SummaryWorkflowParticipant
	confirmTimeoutHours int
	originChannelID     string
	originChannelType   int
	idempotencyKey      string
	agentSessionID      string
	inferred            bool
	target              SummaryWorkflowTarget
}

// CreateFromLegacyHTTP preserves the existing POST /summaries semantics while
// moving persistence out of the Handler. It intentionally does not represent
// Agent authorization or team confirmation. Future Agent tools must add
// policy-gated methods in this package and reuse normalize/persist rather than
// call this compatibility entry point.
//
// This method never performs network I/O. The Handler dispatches WorkerTrigger
// after it has durably bound the workflow result to its caller state. Pending
// replays may intentionally return a trigger for recovery.
func (s *SummaryWorkflowService) CreateFromLegacyHTTP(ctx context.Context, in LegacyCreateSummaryWorkflowInput) (CreateSummaryWorkflowResult, error) {
	var zero CreateSummaryWorkflowResult
	if s == nil || s.db == nil {
		return zero, errors.New("summary workflow service database is required")
	}

	normalized, err := s.normalize(in, s.defaultTimeRangeDays)
	if err != nil {
		return zero, err
	}
	return s.persistIdempotently(ctx, normalized, canonicalSummaryWorkflowRequestHash(normalized))
}

// CreatePersonalFromAgent creates the formal single-user workflow selected by
// the summary workspace policy. The method is intentionally separate from the
// legacy HTTP adapter so the security invariants are compiler-visible:
// authenticated actor == creator, a source is required, no collaborators are
// accepted, and an idempotency key is mandatory.
func (s *SummaryWorkflowService) CreatePersonalFromAgent(ctx context.Context, in AgentCreateSummaryWorkflowInput) (CreateSummaryWorkflowResult, error) {
	if len(deduplicateWorkflowParticipants(in.Participants, in.ActorID)) != 0 {
		return CreateSummaryWorkflowResult{}, NewBizError(40001, "personal workflow cannot include other participants", http.StatusBadRequest)
	}
	return s.createFromAgent(ctx, in, SummaryWorkflowPersonal)
}

// CreateTeamFromAgent creates a multi-user workflow. A shared source is
// optional because invited participants may contribute source material from
// their own authorised scope. The side-effect boundary still requires at
// least one other participant and a non-empty requirement.
func (s *SummaryWorkflowService) CreateTeamFromAgent(ctx context.Context, in AgentCreateSummaryWorkflowInput) (CreateSummaryWorkflowResult, error) {
	if err := validateAgentWorkflowParticipants(in.Participants); err != nil {
		return CreateSummaryWorkflowResult{}, err
	}
	if len(deduplicateWorkflowParticipants(in.Participants, in.ActorID)) == 0 {
		return CreateSummaryWorkflowResult{}, NewBizError(40001, "team workflow requires at least one other participant", http.StatusBadRequest)
	}
	if strings.TrimSpace(in.Requirement) == "" {
		return CreateSummaryWorkflowResult{}, NewBizError(40001, "team workflow requires a summary requirement", http.StatusBadRequest)
	}
	return s.createFromAgent(ctx, in, SummaryWorkflowTeam)
}

func validateAgentWorkflowParticipants(participants []SummaryWorkflowParticipant) *BizError {
	for _, participant := range participants {
		if strings.TrimSpace(participant.UserID) == "" {
			return NewBizError(40001, "each participant requires user_id", http.StatusBadRequest)
		}
	}
	return nil
}

func (s *SummaryWorkflowService) createFromAgent(ctx context.Context, in AgentCreateSummaryWorkflowInput, expectedTarget SummaryWorkflowTarget) (CreateSummaryWorkflowResult, error) {
	var zero CreateSummaryWorkflowResult
	if s == nil || s.db == nil {
		return zero, errors.New("summary workflow service database is required")
	}
	if strings.TrimSpace(in.ActorID) == "" {
		return zero, NewBizError(40100, "actor is required", http.StatusUnauthorized)
	}
	if strings.TrimSpace(in.SpaceID) == "" {
		return zero, NewBizError(40001, "space_id is required", http.StatusBadRequest)
	}
	if !ValidSummaryWorkflowIdempotencyKey(strings.TrimSpace(in.IdempotencyKey)) {
		return zero, NewBizError(40005, "valid Idempotency-Key is required", http.StatusBadRequest)
	}
	sources := deduplicateWorkflowSources(in.Sources)
	if expectedTarget == SummaryWorkflowPersonal && len(sources) == 0 {
		return zero, NewBizError(40001, "at least one authorised source is required", http.StatusBadRequest)
	}
	if err := validateAgentWorkflowSources(sources); err != nil {
		return zero, err
	}
	if in.TimeRange == nil && s.maxTimeRangeDays > 0 && AgentSummaryDefaultTimeRangeDays > s.maxTimeRangeDays {
		return zero, NewBizError(40002, fmt.Sprintf("时间范围不能超过%d天", s.maxTimeRangeDays), http.StatusBadRequest)
	}

	normalized, err := s.normalize(LegacyCreateSummaryWorkflowInput{
		ActorID:             in.ActorID,
		CreatorID:           in.ActorID,
		SpaceID:             in.SpaceID,
		Title:               in.Title,
		Topic:               in.Requirement,
		TimeRange:           in.TimeRange,
		Sources:             sources,
		Participants:        in.Participants,
		ConfirmTimeoutHours: in.ConfirmTimeoutHours,
		OriginChannelID:     in.OriginChannelID,
		OriginChannelType:   in.OriginChannelType,
		IdempotencyKey:      in.IdempotencyKey,
	}, AgentSummaryDefaultTimeRangeDays)
	if err != nil {
		return zero, err
	}
	if normalized.target != expectedTarget {
		return zero, NewBizError(40001, "workflow target does not match confirmed route", http.StatusBadRequest)
	}
	normalized.agentSessionID = strings.TrimSpace(in.AgentSessionID)

	return s.persistIdempotently(ctx, normalized, canonicalSummaryWorkflowRequestHash(normalized))
}

func (s *SummaryWorkflowService) persistIdempotently(ctx context.Context, in normalizedSummaryWorkflowInput, requestHash string) (CreateSummaryWorkflowResult, error) {
	var zero CreateSummaryWorkflowResult
	if in.idempotencyKey != "" {
		result, found, err := s.findIdempotentWorkflow(ctx, in, requestHash)
		if err != nil || found {
			return result, err
		}
	}
	result, err := s.persist(ctx, in, requestHash)
	if errors.Is(err, errSummaryWorkflowIdempotencyRace) {
		result, found, findErr := s.findIdempotentWorkflow(ctx, in, requestHash)
		if findErr != nil {
			return zero, findErr
		}
		if found {
			return result, nil
		}
		return zero, errors.New("summary workflow idempotency binding disappeared after conflict")
	}
	return result, err
}

func validateAgentWorkflowSources(sources []SummaryWorkflowSource) *BizError {
	for _, source := range sources {
		if strings.TrimSpace(source.SourceID) == "" ||
			source.SourceType < model.SourceGroup || source.SourceType > model.SourceDirect {
			return NewBizError(40001, "each source requires source_id and source_type 1, 2, or 3", http.StatusBadRequest)
		}
	}
	return nil
}

func (s *SummaryWorkflowService) normalize(in LegacyCreateSummaryWorkflowInput, defaultTimeRangeDays int) (normalizedSummaryWorkflowInput, error) {
	if strings.TrimSpace(in.SpaceID) == "" {
		return normalizedSummaryWorkflowInput{}, NewBizError(40001, "space_id is required", http.StatusBadRequest)
	}

	idempotencyKey := strings.TrimSpace(in.IdempotencyKey)
	if idempotencyKey != "" && !ValidSummaryWorkflowIdempotencyKey(idempotencyKey) {
		return normalizedSummaryWorkflowInput{}, NewBizError(40005, "invalid Idempotency-Key header", http.StatusBadRequest)
	}

	creatorID := in.CreatorID
	if creatorID == "" {
		creatorID = in.ActorID
	}

	sources := append([]SummaryWorkflowSource(nil), in.Sources...)
	if len(sources) == 0 && in.OriginChannelID != "" &&
		in.OriginChannelType >= model.OriginChannelGroup && in.OriginChannelType <= model.OriginChannelDM {
		sources = []SummaryWorkflowSource{{SourceType: in.OriginChannelType, SourceID: in.OriginChannelID}}
	}

	explicitTimeRange := in.TimeRange != nil
	var timeStart, timeEnd time.Time
	if explicitTimeRange {
		timeStart = in.TimeRange.Start
		timeEnd = in.TimeRange.End
	} else {
		timeEnd = timezone.Now()
		timeStart = timeEnd.Add(-time.Duration(defaultTimeRangeDays) * 24 * time.Hour)
	}

	scope := model.SnapshotScope{ChannelIDs: workflowChannelIDs(sources)}
	if explicitTimeRange {
		scope.TimeRange = model.TimeRangeJSON{
			Start: timeStart.Format(time.RFC3339),
			End:   timeEnd.Format(time.RFC3339),
		}
	}
	if bizErr := ValidatePersonalWorkflow(
		in.ActorID,
		in.Title,
		in.Topic,
		scope,
		len(sources),
		in.OriginChannelID,
		in.OriginChannelType,
		explicitTimeRange,
		timeStart,
		timeEnd,
		s.maxTimeRangeDays,
	); bizErr != nil {
		return normalizedSummaryWorkflowInput{}, bizErr
	}

	confirmTimeoutHours := in.ConfirmTimeoutHours
	if confirmTimeoutHours <= 0 {
		confirmTimeoutHours = 24
	}
	sources = deduplicateWorkflowSources(sources)
	participants := deduplicateWorkflowParticipants(in.Participants, creatorID)
	target := SummaryWorkflowPersonal
	if len(participants) > 0 {
		target = SummaryWorkflowTeam
	}

	return normalizedSummaryWorkflowInput{
		actorID:             in.ActorID,
		creatorID:           creatorID,
		spaceID:             in.SpaceID,
		title:               strings.TrimSpace(in.Title),
		topic:               in.Topic,
		timeStart:           timeStart,
		timeEnd:             timeEnd,
		explicitTimeRange:   explicitTimeRange,
		sources:             sources,
		participants:        participants,
		confirmTimeoutHours: confirmTimeoutHours,
		originChannelID:     in.OriginChannelID,
		originChannelType:   in.OriginChannelType,
		idempotencyKey:      idempotencyKey,
		inferred:            len(sources) == 0,
		target:              target,
	}, nil
}

func (s *SummaryWorkflowService) persist(ctx context.Context, in normalizedSummaryWorkflowInput, requestHash string) (CreateSummaryWorkflowResult, error) {
	taskNo := GenerateTaskNo()
	title := in.title
	if title == "" {
		title = in.topic
	}
	if title == "" {
		title = "总结-" + taskNo[len(taskNo)-8:]
	}

	deadline := timezone.Now().Add(time.Duration(in.confirmTimeoutHours) * time.Hour)
	task := model.SummaryTask{
		TaskNo:            taskNo,
		SpaceID:           in.spaceID,
		CreatorID:         in.creatorID,
		Title:             title,
		Topic:             in.topic,
		SummaryMode:       model.ModeByPerson,
		TimeRangeStart:    in.timeStart,
		TimeRangeEnd:      in.timeEnd,
		Status:            model.StatusPending,
		TriggerType:       model.TriggerManual,
		ConfirmDeadline:   &deadline,
		OriginChannelID:   in.originChannelID,
		OriginChannelType: in.originChannelType,
		AgentSessionID:    in.agentSessionID,
	}

	var creatorParticipantID int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&task).Error; err != nil {
			return err
		}
		for _, source := range in.sources {
			row := model.SummarySource{
				TaskID:     task.ID,
				SourceType: source.SourceType,
				SourceID:   source.SourceID,
				SourceName: ResolveSourceNameWithType(source.SourceID, source.SourceType, s.imDB),
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}

		now := timezone.Now()
		creator := model.SummaryParticipant{
			TaskID:      task.ID,
			UserID:      in.creatorID,
			UserName:    ResolveUserName(in.creatorID),
			Status:      model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		if err := tx.Create(&creator).Error; err != nil {
			return err
		}
		personal := model.PersonalResult{
			TaskID:           task.ID,
			ParticipantRefID: creator.ID,
			UserID:           in.creatorID,
			WorkerStatus:     model.PersonalStatusPending,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := tx.Create(&personal).Error; err != nil {
			return err
		}
		if err := tx.Model(&creator).Update("personal_result_id", personal.ID).Error; err != nil {
			return err
		}
		creatorParticipantID = creator.ID

		for _, participant := range in.participants {
			name := participant.UserName
			if name == "" {
				name = ResolveUserName(participant.UserID)
			}
			row := model.SummaryParticipant{
				TaskID:   task.ID,
				UserID:   participant.UserID,
				UserName: name,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}

		if in.idempotencyKey != "" {
			binding := model.SummaryWorkflowIdempotency{
				SpaceID:        in.spaceID,
				UserID:         in.actorID,
				IdempotencyKey: in.idempotencyKey,
				RequestHash:    requestHash,
				TaskID:         task.ID,
				CreatedAt:      now,
			}
			if err := createSummaryWorkflowIdempotencyBinding(tx, &binding); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return CreateSummaryWorkflowResult{}, err
	}

	trigger := &model.WorkerTriggerRequest{
		Type:             "personal_summary",
		TaskID:           task.ID,
		ParticipantRefID: creatorParticipantID,
	}
	return CreateSummaryWorkflowResult{
		Task:                 task,
		CreatorParticipantID: creatorParticipantID,
		Target:               in.target,
		Inferred:             in.inferred,
		WorkerTrigger:        trigger,
	}, nil
}

func (s *SummaryWorkflowService) findIdempotentWorkflow(ctx context.Context, in normalizedSummaryWorkflowInput, requestHash string) (CreateSummaryWorkflowResult, bool, error) {
	var binding model.SummaryWorkflowIdempotency
	err := s.db.WithContext(ctx).
		Where("space_id = ? AND user_id = ? AND idempotency_key = ?", in.spaceID, in.actorID, in.idempotencyKey).
		Take(&binding).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return CreateSummaryWorkflowResult{}, false, nil
		}
		return CreateSummaryWorkflowResult{}, false, err
	}
	if binding.RequestHash != requestHash {
		return CreateSummaryWorkflowResult{}, false, &SummaryWorkflowIdempotencyError{
			ExistingTaskID: binding.TaskID,
			Reason:         "request_mismatch",
			RecoveryAction: "open_existing_summary",
			BizError: NewBizError(
				WorkflowIdempotencyMismatchCode,
				"idempotency key already used for a different request",
				http.StatusConflict,
			),
		}
	}

	var task model.SummaryTask
	if err := s.db.WithContext(ctx).
		Where("id = ? AND space_id = ? AND creator_id = ? AND deleted_at IS NULL", binding.TaskID, in.spaceID, in.creatorID).
		Take(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return CreateSummaryWorkflowResult{}, false, &SummaryWorkflowIdempotencyError{
				ExistingTaskID: binding.TaskID,
				Reason:         "deleted_summary",
				RecoveryAction: "start_new_summary",
				BizError: NewBizError(
					WorkflowIdempotencyMismatchCode,
					"idempotency key is bound to a deleted or unavailable summary",
					http.StatusConflict,
				),
			}
		}
		return CreateSummaryWorkflowResult{}, false, fmt.Errorf("load idempotent summary task %d: %w", binding.TaskID, err)
	}
	result := CreateSummaryWorkflowResult{
		Task:     task,
		Target:   in.target,
		Inferred: in.inferred,
		Replayed: true,
	}
	if task.Status == model.StatusPending || task.Status == model.StatusProcessing || task.Status == model.StatusWaitingConfirm {
		var creator model.SummaryParticipant
		if err := s.db.WithContext(ctx).
			Where("task_id = ? AND user_id = ?", task.ID, in.creatorID).
			Take(&creator).Error; err != nil {
			return CreateSummaryWorkflowResult{}, false, fmt.Errorf("load idempotent summary creator %d: %w", task.ID, err)
		}
		var personal model.PersonalResult
		if err := s.db.WithContext(ctx).
			Where("task_id = ? AND participant_ref_id = ?", task.ID, creator.ID).
			Take(&personal).Error; err != nil {
			return CreateSummaryWorkflowResult{}, false, fmt.Errorf("load idempotent personal result %d: %w", task.ID, err)
		}
		if personal.WorkerStatus == model.PersonalStatusPending {
			result.CreatorParticipantID = creator.ID
			result.WorkerTrigger = &model.WorkerTriggerRequest{
				Type:             "personal_summary",
				TaskID:           task.ID,
				ParticipantRefID: creator.ID,
			}
		}
	}
	return result, true, nil
}

// createSummaryWorkflowIdempotencyBinding inserts the unique binding and then
// locks and reads it back to identify the winner. RowsAffected is deliberately
// ignored: MySQL with clientFoundRows=true can report a no-op conflict update
// as one affected row.
func createSummaryWorkflowIdempotencyBinding(tx *gorm.DB, binding *model.SummaryWorkflowIdempotency) error {
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(binding).Error; err != nil {
		return fmt.Errorf("create summary workflow idempotency: %w", err)
	}

	var persisted model.SummaryWorkflowIdempotency
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("space_id = ? AND user_id = ? AND idempotency_key = ?", binding.SpaceID, binding.UserID, binding.IdempotencyKey).
		Take(&persisted).Error; err != nil {
		return fmt.Errorf("read back summary workflow idempotency: %w", err)
	}
	if persisted.TaskID != binding.TaskID {
		return errSummaryWorkflowIdempotencyRace
	}
	return nil
}

func workflowChannelIDs(sources []SummaryWorkflowSource) []string {
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		if source.SourceID != "" {
			ids = append(ids, source.SourceID)
		}
	}
	return ids
}

func deduplicateWorkflowSources(sources []SummaryWorkflowSource) []SummaryWorkflowSource {
	if len(sources) < 2 {
		return sources
	}
	out := make([]SummaryWorkflowSource, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		key := fmt.Sprintf("%d:%s", source.SourceType, source.SourceID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, source)
	}
	return out
}

func deduplicateWorkflowParticipants(participants []SummaryWorkflowParticipant, creatorID string) []SummaryWorkflowParticipant {
	out := make([]SummaryWorkflowParticipant, 0, len(participants))
	seen := map[string]struct{}{creatorID: {}}
	for _, participant := range participants {
		if _, exists := seen[participant.UserID]; exists {
			continue
		}
		seen[participant.UserID] = struct{}{}
		out = append(out, participant)
	}
	return out
}

func canonicalSummaryWorkflowRequestHash(in normalizedSummaryWorkflowInput) string {
	sources := append([]SummaryWorkflowSource(nil), in.sources...)
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].SourceType != sources[j].SourceType {
			return sources[i].SourceType < sources[j].SourceType
		}
		return sources[i].SourceID < sources[j].SourceID
	})
	participants := append([]SummaryWorkflowParticipant(nil), in.participants...)
	sort.Slice(participants, func(i, j int) bool {
		if participants[i].UserID != participants[j].UserID {
			return participants[i].UserID < participants[j].UserID
		}
		return participants[i].UserName < participants[j].UserName
	})

	timeRangeMode := "default"
	timeStart := ""
	timeEnd := ""
	if in.explicitTimeRange {
		timeRangeMode = "explicit"
		timeStart = in.timeStart.UTC().Format(time.RFC3339Nano)
		timeEnd = in.timeEnd.UTC().Format(time.RFC3339Nano)
	}
	payload := struct {
		CreatorID           string                       `json:"creator_id"`
		Title               string                       `json:"title"`
		Topic               string                       `json:"topic"`
		TimeRangeMode       string                       `json:"time_range_mode"`
		TimeStart           string                       `json:"time_start"`
		TimeEnd             string                       `json:"time_end"`
		Sources             []SummaryWorkflowSource      `json:"sources"`
		Participants        []SummaryWorkflowParticipant `json:"participants"`
		ConfirmTimeoutHours int                          `json:"confirm_timeout_hours"`
		OriginChannelID     string                       `json:"origin_channel_id"`
		OriginChannelType   int                          `json:"origin_channel_type"`
		AgentSessionID      string                       `json:"agent_session_id,omitempty"`
	}{
		CreatorID:           in.creatorID,
		Title:               strings.TrimSpace(in.title),
		Topic:               strings.TrimSpace(in.topic),
		TimeRangeMode:       timeRangeMode,
		TimeStart:           timeStart,
		TimeEnd:             timeEnd,
		Sources:             sources,
		Participants:        participants,
		ConfirmTimeoutHours: in.confirmTimeoutHours,
		OriginChannelID:     in.originChannelID,
		OriginChannelType:   in.originChannelType,
		AgentSessionID:      in.agentSessionID,
	}
	data, _ := json.Marshal(payload)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
