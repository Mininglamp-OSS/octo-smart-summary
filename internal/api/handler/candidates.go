package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// CandidateHandler handles member and chat candidate search.
type CandidateHandler struct {
	imDB *gorm.DB
}

// NewCandidateHandler creates a new CandidateHandler.
func NewCandidateHandler(imDB *gorm.DB) *CandidateHandler {
	return &CandidateHandler{imDB: imDB}
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
	q.Limit(20).Find(&users)

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
				Where("gm.uid = ?", currentUIDStr)
		}
		if spaceIDStr != "" {
			q = q.Where("g.space_id = ?", spaceIDStr)
		}
		if keyword != "" {
			q = q.Where("g.name LIKE ?", "%"+keyword+"%")
		}
		q.Limit(20).Find(&groups)
		for _, g := range groups {
			list = append(list, gin.H{
				"chat_id":      g.GroupNo,
				"chat_type":    "group",
				"name":         g.Name,
				"member_count": nil,
			})
		}
	}

	// --- Threads (子区, channelType=5) ---
	if chatType == "" || chatType == "all" || chatType == "thread" {
		type imThread struct {
			ShortID string `gorm:"column:short_id"`
			Name    string `gorm:"column:name"`
			GroupNo string `gorm:"column:group_no"`
		}
		var threads []imThread
		q := h.imDB.Table("thread t").
			Select("t.short_id, t.name, t.group_no").
			Joins("INNER JOIN `group` g ON g.group_no COLLATE utf8mb4_unicode_ci = t.group_no").
			Where("t.status = 1 AND g.status = 1")
		if currentUIDStr != "" {
			q = q.Joins("INNER JOIN thread_member tm ON tm.thread_id = t.id").
				Where("tm.uid = ?", currentUIDStr)
		}
		if spaceIDStr != "" {
			q = q.Where("g.space_id = ?", spaceIDStr)
		}
		if keyword != "" {
			q = q.Where("t.name LIKE ? OR g.name LIKE ?", "%"+keyword+"%", "%"+keyword+"%")
		}
		q.Limit(20).Find(&threads)
		for _, t := range threads {
			list = append(list, gin.H{
				"chat_id":         t.GroupNo + "____" + t.ShortID,
				"chat_type":       "thread",
				"name":            t.Name,
				"member_count":    nil,
				"parent_group_no": t.GroupNo,
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
			Select("ce.channel_id, u.name").
			Joins("LEFT JOIN user u ON ce.channel_id = u.uid").
			Where("ce.uid = ? AND ce.channel_type = 1", currentUIDStr).
			// Exclude known system accounts by name; creator_uid != '' in the robot
			// subquery is a catch-all that filters any unlisted system bots whose
			// creator_uid is empty.
			Where("ce.channel_id NOT IN ('fileHelper', 'botfather')").
			// space_member filter is inside the human branch only — bots may not
			// appear in space_member and must not be excluded by it.
			Where(`(
				(LENGTH(ce.channel_id) = 32 AND u.robot = 0
					AND (? = '' OR ce.channel_id IN (SELECT uid FROM space_member WHERE space_id = ? AND status = 1)))
				OR
				ce.channel_id IN (
					SELECT f.to_uid FROM friend f
					WHERE f.uid = ? AND f.is_deleted = 0
					AND f.to_uid IN (SELECT robot_id FROM robot WHERE creator_uid != '')
				)
			)`, spaceIDStr, spaceIDStr, currentUIDStr)
		if keyword != "" {
			q = q.Where("u.name LIKE ?", "%"+keyword+"%")
		}
		q.Order("ce.updated_at DESC").Limit(20).Find(&directs)
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
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}
