package handler

import (
	"net/http"
	"sort"

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
//   - filter:    "followed" | "recent" | "" (empty = all, backward compatible)
//
// Groups are fetched from the `group` table.
// Direct chats are fetched from `conversation_extra` (channel_type=1), filtered to
// human peers only (32-char hex uid, not in robot table, robot flag = 0).
//
// When filter=followed, only groups where group_setting.save=1 for the current user
// are returned (plus their threads). Direct chats are excluded.
// When filter=recent, conversations are fetched from conversation_extra ordered by
// updated_at DESC, returning recently active groups and directs.
func (h *CandidateHandler) SearchChatCandidates(c *gin.Context) {
	if h.imDB == nil {
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": []interface{}{}})
		return
	}

	keyword := c.Query("keyword")
	chatType := c.Query("chat_type") // "group", "direct", or "" = all
	filter := c.Query("filter")     // "followed", "recent", or "" = all

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

	// --- Build followed group_no set (used by filter=followed and for is_followed enrichment) ---
	followedGroupNos := map[string]bool{}
	if currentUIDStr != "" {
		type followedGroup struct {
			GroupNo string `gorm:"column:group_no"`
		}
		var fgs []followedGroup
		h.imDB.Table("group_setting").
			Select("group_no").
			Where("uid = ? AND save = 1", currentUIDStr).
			Find(&fgs)
		for _, fg := range fgs {
			followedGroupNos[fg.GroupNo] = true
		}
	}

	// --- Build recent channel maps (used by filter=recent and for last_active_at enrichment) ---
	// conversation_extra mixes channel types under one channel_id column, so split by
	// channel_type to avoid cross-type collisions. Thread channel_ids use the composite
	// format 'group_no____short_id' (4 underscores), distinct from a bare group_no.
	recentGroups := map[string]string{}  // channel_type=2: group_no       -> updated_at
	recentThreads := map[string]string{} // channel_type=5: group_no____short_id -> updated_at
	recentDirects := map[string]string{} // channel_type=1: peer uid       -> updated_at
	if currentUIDStr != "" {
		type recentConv struct {
			ChannelID   string `gorm:"column:channel_id"`
			ChannelType int    `gorm:"column:channel_type"`
			UpdatedAt   string `gorm:"column:updated_at"`
		}
		var rcs []recentConv
		h.imDB.Table("conversation_extra").
			Select("channel_id, channel_type, updated_at").
			Where("uid = ?", currentUIDStr).
			Order("updated_at DESC").
			Limit(50).
			Find(&rcs)
		for _, rc := range rcs {
			switch rc.ChannelType {
			case 1:
				recentDirects[rc.ChannelID] = rc.UpdatedAt
			case 5:
				recentThreads[rc.ChannelID] = rc.UpdatedAt
			default: // channel_type=2 (group)
				recentGroups[rc.ChannelID] = rc.UpdatedAt
			}
		}
	}

	// --- Determine which sections to include based on filter ---
	includeGroups := chatType == "" || chatType == "all" || chatType == "group"
	includeThreads := chatType == "" || chatType == "all" || chatType == "thread"
	includeDirects := chatType == "" || chatType == "all" || chatType == "direct"

	if filter == "followed" {
		// Followed is group-centric: only groups + their threads, no directs.
		includeDirects = false
	} else if filter == "recent" {
		// Recent includes both groups and directs that appear in conversation_extra.
	}

	// --- Groups ---
	if includeGroups {
		var groups []imGroup
		q := h.imDB.Table("`group` g").Select("g.group_no, g.name")
		if currentUIDStr != "" {
			q = q.Joins("INNER JOIN group_member gm ON gm.group_no = g.group_no").
				Where("gm.uid = ? AND gm.is_deleted = 0", currentUIDStr)
		}
		if filter == "followed" && currentUIDStr != "" {
			q = q.Joins("INNER JOIN group_setting gs ON gs.group_no = g.group_no").
				Where("gs.uid = ? AND gs.save = 1", currentUIDStr)
		} else if filter == "recent" {
			// Only include groups that appear in the recent conversation list.
			recentGroupNos := make([]string, 0, len(recentGroups))
			for chID := range recentGroups {
				recentGroupNos = append(recentGroupNos, chID)
			}
			if len(recentGroupNos) > 0 {
				q = q.Where("g.group_no IN ?", recentGroupNos)
			} else {
				// No recent conversations — skip group query entirely.
				groups = nil
				goto groupsDone
			}
		}
		if spaceIDStr != "" {
			q = q.Where("g.space_id = ?", spaceIDStr)
		}
		if keyword != "" {
			q = q.Where("g.name LIKE ?", "%"+keyword+"%")
		}
		h.applyLimit(q).Find(&groups)
	groupsDone:
		for _, g := range groups {
			entry := gin.H{
				"chat_id":        g.GroupNo,
				"chat_type":      "group",
				"name":           g.Name,
				"member_count":   nil,
				"is_followed":    followedGroupNos[g.GroupNo],
				"last_active_at": recentGroups[g.GroupNo],
			}
			list = append(list, entry)
		}
	}

	// --- Threads (子区, channelType=5) ---
	if includeThreads {
		type imThread struct {
			ShortID string `gorm:"column:short_id"`
			Name    string `gorm:"column:name"`
			GroupNo string `gorm:"column:group_no"`
		}
		var threads []imThread
		q := h.imDB.Table("thread t").
			Select("DISTINCT t.short_id, t.name, t.group_no").
			Joins("INNER JOIN `group` g ON g.group_no" + h.collate + " = t.group_no").
			Where("t.status = 1 AND g.status = 1 AND t.message_count > 0")
		if currentUIDStr != "" {
			// Use group_member instead of thread_member so that all threads in the
			// user's groups are returned, not just threads the user has posted in.
			q = q.Joins("INNER JOIN group_member gm ON gm.group_no" + h.collate + " = t.group_no").
				Where("gm.uid = ? AND gm.is_deleted = 0", currentUIDStr)
		}
		if filter == "followed" && currentUIDStr != "" {
			// Only threads under followed groups.
			q = q.Joins("INNER JOIN group_setting gs2 ON gs2.group_no" + h.collate + " = t.group_no").
				Where("gs2.uid = ? AND gs2.save = 1", currentUIDStr)
		} else if filter == "recent" {
			// Recent threads are matched by their full composite channel id
			// ('group_no____short_id') against recentThreads, NOT by parent group_no —
			// a recently active parent group does not make all its threads recent, and a
			// recently active thread may live under a group that itself is not recent.
			// The composite id is not a SQL column, so the match is applied in the loop
			// below. If there are no recent threads at all, skip the query entirely.
			if len(recentThreads) == 0 {
				threads = nil
				goto threadsDone
			}
		}
		if spaceIDStr != "" {
			q = q.Where("g.space_id = ?", spaceIDStr)
		}
		if keyword != "" {
			q = q.Where("t.name LIKE ? OR g.name LIKE ?", "%"+keyword+"%", "%"+keyword+"%")
		}
		h.applyLimit(q).Find(&threads)
	threadsDone:
		for _, t := range threads {
			compositeID := t.GroupNo + "____" + t.ShortID
			// For filter=recent, only surface threads whose own composite channel id
			// appears in the recent conversation list.
			if filter == "recent" {
				if _, ok := recentThreads[compositeID]; !ok {
					continue
				}
			}
			list = append(list, gin.H{
				"chat_id":         compositeID,
				"chat_type":       "thread",
				"name":            t.Name,
				"member_count":    nil,
				"parent_group_no": t.GroupNo,
				"is_followed":     followedGroupNos[t.GroupNo],
				"last_active_at":  recentThreads[compositeID],
			})
		}
	}

	// --- Direct chats ---
	// Source: conversation_extra where channel_type=1 (P2P) for the current user.
	// channel_id in P2P conversations is the peer's uid.
	// Filter: only 32-char hex uids (excludes system accounts like fileHelper/botfather),
	//         not a robot (robot table + robot flag).
	// Requires authentication; skipped silently if unauthenticated.
	if includeDirects && currentUIDStr != "" {
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
			// For filter=recent, skip directs not in the recent conversations list.
			if filter == "recent" {
				if _, ok := recentDirects[d.ChannelID]; !ok {
					continue
				}
			}
			list = append(list, gin.H{
				"chat_id":        d.ChannelID,
				"chat_type":      "direct",
				"name":           name,
				"member_count":   nil,
				"is_bot":         d.Robot == 1,
				"last_active_at": recentDirects[d.ChannelID],
			})
		}
	}

	// --- Sort by recency when filter=recent ---
	if filter == "recent" && len(list) > 1 {
		sort.SliceStable(list, func(i, j int) bool {
			ai, _ := list[i]["last_active_at"].(string)
			aj, _ := list[j]["last_active_at"].(string)
			if ai == "" {
				return false
			}
			if aj == "" {
				return true
			}
			return ai > aj // DESC: newer first
		})
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}
