package handler

import (
	"log"
	"net/http"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/ws"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
)

// InternalHandler handles internal endpoints (Worker → API callbacks).
type InternalHandler struct {
	hub       *ws.Hub
	triggerCh chan<- model.WorkerTriggerRequest
}

// NewInternalHandler creates a new InternalHandler.
func NewInternalHandler(hub *ws.Hub) *InternalHandler {
	return &InternalHandler{hub: hub}
}

// SetTriggerCh sets the worker trigger channel (called when worker is available).
func (h *InternalHandler) SetTriggerCh(ch chan<- model.WorkerTriggerRequest) {
	h.triggerCh = ch
}

// WorkerTrigger handles POST /internal/worker-trigger
func (h *InternalHandler) WorkerTrigger(c *gin.Context) {
	var req model.WorkerTriggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if h.triggerCh == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker not available"})
		return
	}

	select {
	case h.triggerCh <- req:
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "triggered"})
	default:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "trigger queue full"})
	}
}

// TaskEvent handles POST /internal/task-event
func (h *InternalHandler) TaskEvent(c *gin.Context) {
	var event model.TaskEvent
	if err := c.ShouldBindJSON(&event); err != nil {
		log.Printf("[handler] TaskEvent bind error: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Determine event type for WS push
	wsEventType := "TASK_STATUS_CHANGED"
	if event.EventType != "" {
		wsEventType = event.EventType
	}

	payload := gin.H{
		"type": wsEventType,
		"payload": gin.H{
			"task_id":  event.TaskID,
			"status":   event.Status,
			"progress": event.Progress,
			"message":  event.Message,
		},
	}

	// Directed push to specific user, or broadcast to all subscribers
	if event.TargetUserID != "" {
		h.hub.BroadcastToUser(event.TaskID, event.TargetUserID, payload)
	} else {
		h.hub.Broadcast(event.TaskID, payload)
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok"})
}
