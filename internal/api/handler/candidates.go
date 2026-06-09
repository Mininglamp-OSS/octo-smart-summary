package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// CandidateHandler handles member and chat candidate search.
type CandidateHandler struct {
	imDB       *gorm.DB
	queryLimit int    // -1 = no limit, >0 = SQL LIMIT value
	collate    string // SQL COLLATE clause for cross-table joins (MySQL collation mismatch)
}

// NewCandidateHandler creates a new CandidateHandler.
func NewCandidateHandler(imDB *gorm.DB, queryLimit int) *CandidateHandler {
	return &CandidateHandler{imDB: imDB, queryLimit: queryLimit, collate: " COLLATE utf8mb4_unicode_ci"}
}

func (h *CandidateHandler) applyLimit(q *gorm.DB) *gorm.DB {
	if h.queryLimit > 0 {
		return q.Limit(h.queryLimit)
	}
	return q
}

// imUser holds basic user info from IM DB.
type imUser struct {
	UID  string `gorm:"column:uid"`
	Name string `gorm:"column:name"`
}

func (imUser) TableName() string { return "user" }

// SearchCandidates handles GET /api/v1/summary-member-candidates
// Returns human members of the current Space only (excludes bots).
// Falls back to all human users if no space_id is available.
func (h *CandidateHandler) SearchCandidates(c *gin.Context) {
	if h.imDB == nil {
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": []interface{}{}})
		return
	}
	keyword := c.Query("keyword")
	// Space ID: prefer explicit query param (sent by frontend), fallback to middleware context.
	spaceIDStr := c.Query("space_id")
	if spaceIDStr == "" {
		v, _ := c.Get("space_id")
		spaceIDStr, _ = v.(string)
	}

	// Resolve current user (set by AuthMiddleware via token)
	currentUID, _ := c.Get("user_id")
	currentUIDStr, _ := currentUID.(string)

	var users []imUser

	// Common bot-exclusion conditions:
	// 1. user.robot = 1 (flag on user row)
	// 2. uid in robot table (some system bots have robot=0, e.g. BotFather)
	// 3. uid is not a 32-char hex string (system accounts like fileHelper/botfather)
	q := h.imDB.Table("user u").Select("u.uid, u.name").
		Where("u.robot = 0").
		Where("u.uid NOT IN (SELECT robot_id FROM robot)").
		Where("LENGTH(u.uid) = 32")

	if spaceIDStr != "" {
		// Filter by Space members
		q = q.
			Joins("INNER JOIN space_member sm ON sm.uid = u.uid").
			Where("sm.space_id = ? AND sm.status = 1", spaceIDStr)
	}

	// Exclude the currently logged-in user (task creator doesn't add themselves)
	if currentUIDStr != "" {
		q = q.Where("u.uid != ?", currentUIDStr)
	}

	if keyword != "" {
		q = q.Where("u.name LIKE ? OR u.username LIKE ?", "%"+keyword+"%", "%"+keyword+"%")
	}
	h.applyLimit(q).Find(&users)

	list := make([]gin.H, 0, len(users))
	for _, u := range users {
		list = append(list, gin.H{"user_id": u.UID, "name": u.Name, "avatar": "", "department": ""})
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}

// imGroup holds basic group info from IM DB.
type imGroup struct {
	GroupNo string `gorm:"column:group_no"`
	Name    string `gorm:"column:name"`
}

func (imGroup) TableName() string { return "`group`" }

// imDirect holds a resolved direct-chat peer from conversation_extra.
type imDirect struct {
	ChannelID string `gorm:"column:channel_id"`
	Name      string `gorm:"column:name"`
	Robot     int    `gorm:"column:robot"`
}

// SearchChatCandidates handles GET /api/v1/summary-chat-candidates
// Query params:
//   - keyword:   optional search keyword
//   - chat_type: "group" | "direct" | "" (empty = all)
//
// Groups are fetched from the `group` table.
// Direct chats are fetched from `conversation_extra` (channel_type=1), filtered to
// human peers only (32-char hex uid, not in robot table, robot flag = 0).
func (h *CandidateHandler) SearchChatCandidates(c *gin.Context) {
	if h.imDB == nil {
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": []interface{}{}})
		return
	}

	keyword := c.Query("keyword")
	chatType := c.Query("chat_type") // "group", "direct", or "" = all
	// include_archived: when true, threads with status=2 (Archived) are also
	// returned. Defaults to false. Deleted threads (status=3) are NEVER returned.
	includeArchived := c.Query("include_archived") == "true" || c.Query("include_archived") == "1"

	// Resolve current user from context (set by AuthMiddleware).
	currentUID, _ := c.Get("user_id")
	currentUIDStr, _ := currentUID.(string)

	list := make([]gin.H, 0)

	// Space ID: prefer explicit query param, fallback to middleware context.
	spaceIDStr := c.Query("space_id")
	if spaceIDStr == "" {
		v, _ := c.Get("space_id")
		spaceIDStr, _ = v.(string)
	}

	// --- Groups ---
	if chatType == "" || chatType == "all" || chatType == "group" {
		var groups []imGroup
		q := h.imDB.Table("`group` g").Select("g.group_no, g.name")
		if currentUIDStr != "" {
			q = q.Joins("INNER JOIN group_member gm ON gm.group_no = g.group_no").
				Where("gm.uid = ? AND gm.is_deleted = 0", currentUIDStr)
		}
		if spaceIDStr != "" {
			q = q.Where("g.space_id = ?", spaceIDStr)
		}
		if keyword != "" {
			q = q.Where("g.name LIKE ?", "%"+keyword+"%")
		}
		h.applyLimit(q).Find(&groups)
		for _, g := range groups {
			list = append(list, gin.H{
				"chat_id":      g.GroupNo,
				"chat_type":    "group",
				"name":         g.Name,
				"member_count": nil,
				"is_archived":  false,
			})
		}
	}

	// --- Threads (子区, channelType=5) ---
	if chatType == "" || chatType == "all" || chatType == "thread" {
		type imThread struct {
			ShortID string `gorm:"column:short_id"`
			Name    string `gorm:"column:name"`
			GroupNo string `gorm:"column:group_no"`
			Status  int    `gorm:"column:status"`
		}
		var threads []imThread
		q := h.imDB.Table("thread t").
			Select("DISTINCT t.short_id, t.name, t.group_no, t.status").
			Joins("INNER JOIN `group` g ON g.group_no" + h.collate + " = t.group_no")
		// Default: only Active threads (status=1). When include_archived is set,
		// also return Archived threads (status=2). Deleted (status=3) is never
		// included. Parent group must be active (g.status=1) and the thread must
		// have messages.
		if includeArchived {
			q = q.Where("t.status IN (1, 2) AND g.status = 1 AND t.message_count > 0")
		} else {
			q = q.Where("t.status = 1 AND g.status = 1 AND t.message_count > 0")
		}
		if currentUIDStr != "" {
			// Use group_member instead of thread_member so that all threads in the
			// user's groups are returned, not just threads the user has posted in.
			q = q.Joins("INNER JOIN group_member gm ON gm.group_no" + h.collate + " = t.group_no").
				Where("gm.uid = ? AND gm.is_deleted = 0", currentUIDStr)
		}
		if spaceIDStr != "" {
			q = q.Where("g.space_id = ?", spaceIDStr)
		}
		if keyword != "" {
			q = q.Where("t.name LIKE ? OR g.name LIKE ?", "%"+keyword+"%", "%"+keyword+"%")
		}
		h.applyLimit(q).Find(&threads)
		for _, t := range threads {
			list = append(list, gin.H{
				"chat_id":         t.GroupNo + "____" + t.ShortID,
				"chat_type":       "thread",
				"name":            t.Name,
				"member_count":    nil,
				"parent_group_no": t.GroupNo,
				"is_archived":     t.Status == 2,
			})
		}
	}

	// --- Direct chats ---
	// Source: conversation_extra where channel_type=1 (P2P) for the current user.
	// channel_id in P2P conversations is the peer's uid.
	// Filter: only 32-char hex uids (excludes system accounts like fileHelper/botfather),
	//         not a robot (robot table + robot flag).
	// Requires authentication; skipped silently if unauthenticated.
	if (chatType == "" || chatType == "all" || chatType == "direct") && currentUIDStr != "" {
		var directs []imDirect
		q := h.imDB.Table("conversation_extra ce").
			Select("ce.channel_id, u.name, u.robot").
			Joins("LEFT JOIN user u ON ce.channel_id = u.uid").
			Where("ce.uid = ? AND ce.channel_type = 1", currentUIDStr).
			// Exclude known system accounts by name; creator_uid != '' in the robot
			// subquery is a catch-all that filters any unlisted system bots whose
			// creator_uid is empty.
			Where("ce.channel_id NOT IN ('fileHelper', 'botfather')").
			// Both human and bot branches filter by space_member when a space is selected.
			Where(`(
				(LENGTH(ce.channel_id) = 32 AND u.robot = 0
					AND (? = '' OR ce.channel_id IN (SELECT uid FROM space_member WHERE space_id = ? AND status = 1)))
				OR
				ce.channel_id IN (
					SELECT f.to_uid FROM friend f
					INNER JOIN space_member sm ON sm.uid = f.to_uid AND sm.status = 1 AND (? = '' OR sm.space_id = ?)
					WHERE f.uid = ? AND f.is_deleted = 0
					AND f.to_uid IN (SELECT robot_id FROM robot WHERE creator_uid != '' AND status = 1)
				)
			)`, spaceIDStr, spaceIDStr, spaceIDStr, spaceIDStr, currentUIDStr)
		if keyword != "" {
			q = q.Where("u.name LIKE ?", "%"+keyword+"%")
		}
		h.applyLimit(q.Order("ce.updated_at DESC")).Find(&directs)
		for _, d := range directs {
			name := d.Name
			if name == "" {
				name = d.ChannelID
			}
			list = append(list, gin.H{
				"chat_id":      d.ChannelID,
				"chat_type":    "direct",
				"name":         name,
				"member_count": nil,
				"is_bot":       d.Robot == 1,
				"is_archived":  false,
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}
