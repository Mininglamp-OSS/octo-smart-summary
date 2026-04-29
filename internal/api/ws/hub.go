package ws

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

// WebSocket event type constants for P2.
const (
	EventPersonalSummaryStatus = "PERSONAL_SUMMARY_STATUS"
	EventMetaSummaryUpdated    = "META_SUMMARY_UPDATED"
	EventMemberSubmitted       = "MEMBER_SUBMITTED"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// connWrapper adds a write mutex to a websocket connection.
type connWrapper struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	spaceID string
	userID  string
}

func (cw *connWrapper) writeJSON(v interface{}) error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.conn.WriteJSON(v)
}

func (cw *connWrapper) writeMessage(msgType int, data []byte) error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.conn.WriteMessage(msgType, data)
}

// Hub manages WebSocket subscriptions per task_id with space isolation.
type Hub struct {
	mu    sync.RWMutex
	subs  map[int64]map[*connWrapper]bool
	conns map[*connWrapper]bool
	db    *gorm.DB
}

// NewHub creates a new WebSocket hub.
func NewHub(db *gorm.DB) *Hub {
	return &Hub{
		subs:  make(map[int64]map[*connWrapper]bool),
		conns: make(map[*connWrapper]bool),
		db:    db,
	}
}

type wsMessage struct {
	Action  string  `json:"action"`
	TaskIDs []int64 `json:"task_ids,omitempty"`
}

// taskBelongsToSpace verifies that a task belongs to the given space.
func (h *Hub) taskBelongsToSpace(taskID int64, spaceID string) bool {
	if h.db == nil {
		return false
	}
	var count int64
	h.db.Model(&model.SummaryTask{}).
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).
		Count(&count)
	return count > 0
}

// HandleWS is the gin handler for WebSocket connections.
func (h *Hub) HandleWS(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	// Allow empty spaceID: WS will receive all events (no space isolation in that case)

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}

	userID := middleware.GetUserID(c)
	cw := &connWrapper{conn: conn, spaceID: spaceID, userID: userID}

	h.mu.Lock()
	h.conns[cw] = true
	h.mu.Unlock()

	defer func() {
		h.removeConn(cw)
		conn.Close()
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg wsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			_ = cw.writeJSON(gin.H{"error": "invalid JSON"})
			continue
		}

		switch msg.Action {
		case "subscribe":
			var allowed []int64
			var denied []int64
			for _, tid := range msg.TaskIDs {
				if h.taskBelongsToSpace(tid, cw.spaceID) {
					allowed = append(allowed, tid)
				} else {
					denied = append(denied, tid)
				}
			}
			if len(allowed) > 0 {
				h.mu.Lock()
				for _, tid := range allowed {
					if h.subs[tid] == nil {
						h.subs[tid] = make(map[*connWrapper]bool)
					}
					h.subs[tid][cw] = true
				}
				h.mu.Unlock()
			}
			resp := gin.H{"action": "subscribed", "task_ids": allowed}
			if len(denied) > 0 {
				resp["denied_task_ids"] = denied
			}
			_ = cw.writeJSON(resp)

		case "unsubscribe":
			h.mu.Lock()
			for _, tid := range msg.TaskIDs {
				if h.subs[tid] != nil {
					delete(h.subs[tid], cw)
					if len(h.subs[tid]) == 0 {
						delete(h.subs, tid)
					}
				}
			}
			h.mu.Unlock()
			_ = cw.writeJSON(gin.H{"action": "unsubscribed", "task_ids": msg.TaskIDs})

		case "ping":
			_ = cw.writeJSON(gin.H{"action": "pong"})
		}
	}
}

func (h *Hub) removeConn(cw *connWrapper) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, cw)
	for tid, conns := range h.subs {
		delete(conns, cw)
		if len(conns) == 0 {
			delete(h.subs, tid)
		}
	}
}

// BroadcastToUser sends a message to a specific user subscribed to a task.
func (h *Hub) BroadcastToUser(taskID int64, userID string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	h.mu.RLock()
	var targets []*connWrapper
	for cw := range h.subs[taskID] {
		if cw.userID == userID {
			targets = append(targets, cw)
		}
	}
	h.mu.RUnlock()

	var dead []*connWrapper
	for _, cw := range targets {
		if err := cw.writeMessage(websocket.TextMessage, data); err != nil {
			dead = append(dead, cw)
		}
	}
	if len(dead) > 0 {
		for _, cw := range dead {
			h.removeConn(cw)
		}
	}
}

// Broadcast sends a message to all subscribers of a task.
func (h *Hub) Broadcast(taskID int64, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	h.mu.RLock()
	subscribers := make([]*connWrapper, 0, len(h.subs[taskID]))
	for cw := range h.subs[taskID] {
		subscribers = append(subscribers, cw)
	}
	h.mu.RUnlock()

	var dead []*connWrapper
	for _, cw := range subscribers {
		if err := cw.writeMessage(websocket.TextMessage, data); err != nil {
			dead = append(dead, cw)
		}
	}

	if len(dead) > 0 {
		for _, cw := range dead {
			h.removeConn(cw)
		}
	}
}
