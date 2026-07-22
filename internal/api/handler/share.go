package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	maxShareTargets = 30
	maxPreviewRunes = 220
)

type ShareHandler struct {
	db   *gorm.DB
	imDB *gorm.DB
}

func NewShareHandler(db, imDB *gorm.DB) *ShareHandler { return &ShareHandler{db: db, imDB: imDB} }

type shareTarget struct {
	ChannelID   string `json:"channel_id"`
	ChannelType int    `json:"channel_type"`
}

type createSharesRequest struct {
	IdempotencyKey string        `json:"idempotency_key"`
	Targets        []shareTarget `json:"targets"`
}

func (h *ShareHandler) resolveTask(param, spaceID string) (model.SummaryTask, error) {
	var task model.SummaryTask
	q := h.db.Where("space_id = ? AND deleted_at IS NULL", spaceID)
	if id, err := strconv.ParseInt(param, 10, 64); err == nil {
		return task, q.Where("id = ?", id).First(&task).Error
	}
	return task, q.Where("task_no = ?", param).First(&task).Error
}

func normalizeTargets(targets []shareTarget) ([]shareTarget, bool) {
	seen := make(map[string]struct{}, len(targets))
	out := make([]shareTarget, 0, len(targets))
	for _, target := range targets {
		target.ChannelID = strings.TrimSpace(target.ChannelID)
		if target.ChannelID == "" || (target.ChannelType != model.ChannelTypeDM && target.ChannelType != model.ChannelTypeGroup) {
			return nil, false
		}
		key := strconv.Itoa(target.ChannelType) + ":" + target.ChannelID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, target)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ChannelType != out[j].ChannelType {
			return out[i].ChannelType < out[j].ChannelType
		}
		return out[i].ChannelID < out[j].ChannelID
	})
	return out, len(out) > 0 && len(out) <= maxShareTargets
}

func requestHash(taskID int64, targets []shareTarget) string {
	b, _ := json.Marshal(struct {
		TaskID  int64         `json:"task_id"`
		Targets []shareTarget `json:"targets"`
	}{taskID, targets})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newShareID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (h *ShareHandler) activeSpaceMember(spaceID, uid string) bool {
	if h.imDB == nil || spaceID == "" || uid == "" {
		return false
	}
	var count int64
	err := h.imDB.Raw(
		"SELECT COUNT(*) FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1 WHERE sm.space_id = ? AND sm.uid = ? AND sm.status = 1",
		spaceID, uid,
	).Scan(&count).Error
	return err == nil && count > 0
}

func (h *ShareHandler) canUseTarget(spaceID, uid string, target shareTarget) bool {
	if h.imDB == nil {
		return false
	}
	switch target.ChannelType {
	case model.ChannelTypeGroup:
		var count int64
		err := h.imDB.Raw(
			"SELECT COUNT(*) FROM `group` g INNER JOIN space s ON s.space_id = g.space_id AND s.status = 1 INNER JOIN group_member gm ON gm.group_no = g.group_no WHERE g.group_no = ? AND g.space_id = ? AND g.status = 1 AND gm.uid = ? AND gm.is_deleted = 0",
			target.ChannelID, spaceID, uid,
		).Scan(&count).Error
		return err == nil && count > 0
	case model.ChannelTypeDM:
		return target.ChannelID != uid && h.activeSpaceMember(spaceID, uid) && h.activeSpaceMember(spaceID, target.ChannelID)
	default:
		return false
	}
}

func stripCitationTokens(content string, plain []model.Citation, team []model.TeamCitation) string {
	plainSet := make(map[int]struct{}, len(plain))
	teamSet := make(map[int]struct{}, len(team))
	for _, c := range plain {
		plainSet[c.Index] = struct{}{}
	}
	for _, c := range team {
		teamSet[c.Index] = struct{}{}
	}

	var b strings.Builder
	for i := 0; i < len(content); {
		if content[i] != '[' {
			b.WriteByte(content[i])
			i++
			continue
		}
		end := strings.IndexByte(content[i:], ']')
		if end < 0 {
			b.WriteString(content[i:])
			break
		}
		end += i
		token := content[i+1 : end]
		isTeam := strings.HasPrefix(token, "P")
		number := token
		if isTeam {
			number = strings.TrimPrefix(token, "P")
		}
		idx, err := strconv.Atoi(number)
		_, knownPlain := plainSet[idx]
		_, knownTeam := teamSet[idx]
		// A numeric markdown link such as [1](url) is content, not a citation.
		isLink := end+1 < len(content) && content[end+1] == '('
		if err == nil && !isLink && ((!isTeam && knownPlain) || (isTeam && knownTeam)) {
			i = end + 1
			continue
		}
		b.WriteString(content[i : end+1])
		i = end + 1
	}
	return strings.TrimSpace(b.String())
}

func sharePreview(content string) string {
	lines := strings.Split(content, "\n")
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "#>*-+0123456789. ")
		if line == "" || strings.HasPrefix(line, "|") || strings.HasPrefix(line, "---") {
			continue
		}
		parts = append(parts, line)
	}
	text := strings.Join(parts, " ")
	runes := []rune(text)
	if len(runes) > maxPreviewRunes {
		text = string(runes[:maxPreviewRunes]) + "…"
	}
	return text
}

type shareMaterial struct {
	content        string
	plainCitations []model.Citation
	teamCitations  []model.TeamCitation
	messageCount   int
	version        int
}

func (h *ShareHandler) materialFor(task model.SummaryTask, userID string) (shareMaterial, bool) {
	if result, ok := (&TaskHandler{db: h.db}).pickDisplayResult(task.ID); ok && strings.TrimSpace(result.Content) != "" {
		return shareMaterial{result.Content, result.GetCitations(), result.GetTeamCitations(), result.TotalMsgCount, result.Version}, true
	}
	var personal model.PersonalResult
	if err := h.db.Where("task_id = ? AND user_id = ?", task.ID, userID).First(&personal).Error; err != nil || strings.TrimSpace(personal.Content) == "" {
		return shareMaterial{}, false
	}
	return shareMaterial{personal.Content, personal.GetCitations(), nil, personal.MsgCount, 1}, true
}

func (h *ShareHandler) response(snapshot model.SummaryShareSnapshot, grants []model.SummaryShareGrant) gin.H {
	items := make([]gin.H, 0, len(grants))
	for _, grant := range grants {
		items = append(items, gin.H{"share_id": grant.ShareID, "channel_id": grant.ChannelID, "channel_type": grant.ChannelType})
	}
	return gin.H{"snapshot": snapshot, "grants": items}
}

func (h *ShareHandler) Create(c *gin.Context) {
	spaceID, userID := middleware.GetSpaceID(c), middleware.GetUserID(c)
	var req createSharesRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IdempotencyKey) < 8 || len(req.IdempotencyKey) > 64 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid share request"})
		return
	}
	targets, targetsValid := normalizeTargets(req.Targets)
	if !targetsValid {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid share targets"})
		return
	}
	task, err := h.resolveTask(c.Param("id"), spaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}
	if !canAccessTaskDB(h.db, userID, task.ID, task.CreatorID) {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "无权分享此任务"})
		return
	}
	if task.Status != model.StatusCompleted {
		c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "总结尚未完成"})
		return
	}
	for _, target := range targets {
		if !h.canUseTarget(spaceID, userID, target) {
			c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "无权分享到目标会话"})
			return
		}
	}
	hash := requestHash(task.ID, targets)
	var existing model.SummaryShareSnapshot
	if err := h.db.Where("space_id = ? AND creator_id = ? AND idempotency_key = ?", spaceID, userID, req.IdempotencyKey).First(&existing).Error; err == nil {
		if existing.RequestHash != hash {
			c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "幂等键已用于其他分享请求"})
			return
		}
		var grants []model.SummaryShareGrant
		h.db.Where("snapshot_id = ? AND status = ?", existing.ID, model.ShareGrantActive).Find(&grants)
		ok(c, h.response(existing, grants))
		return
	}

	material, found := h.materialFor(task, userID)
	if !found {
		c.JSON(http.StatusUnprocessableEntity, apiResponse{Code: 40010, Message: "没有可分享的总结内容"})
		return
	}
	content := stripCitationTokens(material.content, material.plainCitations, material.teamCitations)
	if !utf8.ValidString(content) || content == "" {
		c.JSON(http.StatusUnprocessableEntity, apiResponse{Code: 40010, Message: "没有可分享的总结内容"})
		return
	}
	var sources []model.SummarySource
	h.db.Where("task_id = ?", task.ID).Find(&sources)
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.SourceName) != "" {
			names = append(names, source.SourceName)
		}
	}
	var participantCount int64
	h.db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND status <> ?", task.ID, model.ParticipantDeclined).Count(&participantCount)
	now := time.Now()
	snapshot := model.SummaryShareSnapshot{
		TaskID: task.ID, TaskNo: task.TaskNo, SpaceID: spaceID, CreatorID: userID,
		IdempotencyKey: req.IdempotencyKey, RequestHash: hash, Title: task.Title,
		SourceName: strings.Join(names, "、"), SourceCount: len(sources), ParticipantCount: int(participantCount),
		MessageCount: material.messageCount, TimeRangeStart: task.TimeRangeStart, TimeRangeEnd: task.TimeRangeEnd,
		SummaryMode: task.SummaryMode, ResultVersion: material.version, Preview: sharePreview(content), Content: content,
		CreatedAt: now, UpdatedAt: now,
	}
	grants := make([]model.SummaryShareGrant, 0, len(targets))
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&snapshot).Error; err != nil {
			return err
		}
		for _, target := range targets {
			shareID, err := newShareID()
			if err != nil {
				return err
			}
			grant := model.SummaryShareGrant{SnapshotID: snapshot.ID, ShareID: shareID, ChannelID: target.ChannelID, ChannelType: target.ChannelType, Status: model.ShareGrantActive, CreatedAt: now, UpdatedAt: now}
			if err := tx.Create(&grant).Error; err != nil {
				return err
			}
			grants = append(grants, grant)
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "创建分享失败"})
		return
	}
	ok(c, h.response(snapshot, grants))
}

func (h *ShareHandler) canReadGrant(spaceID, userID string, snapshot model.SummaryShareSnapshot, grant model.SummaryShareGrant) bool {
	if snapshot.SpaceID != spaceID || grant.Status != model.ShareGrantActive {
		return false
	}
	if grant.ChannelType == model.ChannelTypeDM {
		if userID != snapshot.CreatorID && userID != grant.ChannelID {
			return false
		}
		return h.activeSpaceMember(spaceID, snapshot.CreatorID) && h.activeSpaceMember(spaceID, grant.ChannelID)
	}
	return h.canUseTarget(spaceID, userID, shareTarget{ChannelID: grant.ChannelID, ChannelType: grant.ChannelType})
}

func (h *ShareHandler) sourceAccessible(spaceID, userID string, snapshot model.SummaryShareSnapshot) bool {
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", snapshot.TaskID, spaceID).First(&task).Error; err != nil {
		return false
	}
	return canAccessTaskDB(h.db, userID, task.ID, task.CreatorID)
}

func (h *ShareHandler) Get(c *gin.Context) {
	var grant model.SummaryShareGrant
	if err := h.db.Where("share_id = ?", c.Param("share_id")).First(&grant).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "分享不存在或已失效"})
		return
	}
	var snapshot model.SummaryShareSnapshot
	if err := h.db.First(&snapshot, grant.SnapshotID).Error; err != nil || !h.canReadGrant(middleware.GetSpaceID(c), middleware.GetUserID(c), snapshot, grant) {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "分享不存在或已失效"})
		return
	}
	spaceID, userID := middleware.GetSpaceID(c), middleware.GetUserID(c)
	ok(c, gin.H{
		"share_id":          grant.ShareID,
		"source_accessible": h.sourceAccessible(spaceID, userID, snapshot),
		"snapshot":          snapshot,
	})
}

func (h *ShareHandler) Revoke(c *gin.Context) {
	var grant model.SummaryShareGrant
	if err := h.db.Where("share_id = ?", c.Param("share_id")).First(&grant).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "分享不存在"})
		return
	}
	var snapshot model.SummaryShareSnapshot
	if err := h.db.First(&snapshot, grant.SnapshotID).Error; err != nil || snapshot.SpaceID != middleware.GetSpaceID(c) || snapshot.CreatorID != middleware.GetUserID(c) {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "分享不存在"})
		return
	}
	now := time.Now()
	h.db.Model(&grant).Updates(map[string]any{"status": model.ShareGrantRevoked, "revoked_at": now, "updated_at": now})
	ok(c, gin.H{"share_id": grant.ShareID, "revoked": true})
}
