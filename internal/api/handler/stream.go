package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/streaming"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const sseHeartbeatInterval = 5 * time.Second

type StreamHandler struct {
	db  *gorm.DB
	hub *streaming.Hub
}

func NewStreamHandler(db *gorm.DB, hub *streaming.Hub) *StreamHandler {
	return &StreamHandler{db: db, hub: hub}
}

// Ingest handles POST /internal/summary-stream. It reads worker-produced NDJSON
// over a single chunked request and publishes events to the in-memory SSE hub.
func (h *StreamHandler) Ingest(c *gin.Context) {
	if h == nil || h.hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "summary stream hub not configured"})
		return
	}
	defer c.Request.Body.Close()

	scanner := bufio.NewScanner(c.Request.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev streaming.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			log.Printf("[summary-stream] invalid event: %v", err)
			continue
		}
		h.hub.Publish(ev)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[summary-stream] ingest read error: %v", err)
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok"})
}

// StreamSummary handles GET /api/v1/summaries/:id/stream.
func (h *StreamHandler) StreamSummary(c *gin.Context) {
	if h == nil || h.hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "summary stream hub not configured"})
		return
	}
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || taskID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	spaceID := middleware.GetSpaceID(c)
	userID := middleware.GetUserID(c)
	if spaceID == "" || userID == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "missing auth context"})
		return
	}
	if !h.canAccessTask(taskID, spaceID, userID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}

	scope := streaming.NormalizeScope(c.DefaultQuery("scope", streaming.ScopePersonal))
	targetUserID := userID
	if scope == streaming.ScopeTeam {
		// Team summary is task-level rather than user-level: all authorized
		// participants subscribe to the same stream key.
		targetUserID = ""
	}

	w := c.Writer
	// Keep SSE alive even if a future http.Server WriteTimeout is configured.
	// Streaming endpoints must not inherit finite write deadlines.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.Flush()

	ch, snapshot, done, cancel := h.hub.Subscribe(taskID, scope, targetUserID)
	defer cancel()

	if snapshot != "" {
		_ = writeSSE(w, streaming.EventSnapshot, streaming.Event{Type: streaming.EventSnapshot, TaskID: taskID, Scope: scope, Content: snapshot})
		w.Flush()
	}
	if done {
		_ = writeSSE(w, streaming.EventDone, streaming.Event{Type: streaming.EventDone, TaskID: taskID, Scope: scope, Content: snapshot})
		w.Flush()
		return
	}

	ticker := time.NewTicker(sseHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			w.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSE(w, ev.Type, ev); err != nil {
				return
			}
			w.Flush()
			if ev.Type == streaming.EventDone || ev.Type == streaming.EventError {
				return
			}
		}
	}
}

func (h *StreamHandler) canAccessTask(taskID int64, spaceID, userID string) bool {
	if h == nil || h.db == nil {
		return false
	}
	var count int64
	err := h.db.Raw(`
SELECT COUNT(1)
FROM summary_task t
WHERE t.id = ?
  AND t.space_id = ?
  AND t.deleted_at IS NULL
  AND (
    t.creator_id = ?
    OR EXISTS (
      SELECT 1 FROM summary_participant p
      WHERE p.task_id = t.id AND p.user_id = ?
    )
  )
`, taskID, spaceID, userID, userID).Scan(&count).Error
	return err == nil && count > 0
}

func writeSSE(w http.ResponseWriter, event string, data interface{}) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
