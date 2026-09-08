package handler

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ContentCommandHandler is the coordinated command adapter. It is deliberately
// NOT mounted by the production router until every legacy writer/cleaner and
// the worker poller adopt the protocol. Tests mount it with an isolated fixture
// allowlist; SUMMARY_CONTENT_READ_SPACES must never enable these commands.
type ContentCommandHandler struct {
	boundary *ContentReadHandler
}

func NewContentCommandHandler(db *gorm.DB, writeSpaces string) *ContentCommandHandler {
	return &ContentCommandHandler{boundary: NewContentReadHandler(db, writeSpaces)}
}

func (h *ContentCommandHandler) bind(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4*1024*1024)
	if err := c.ShouldBindJSON(dst); err != nil {
		h.boundary.respond(c, nil, &service.ContentError{Code: "invalid_request", HTTPStatus: 400})
		return false
	}
	return true
}

func (h *ContentCommandHandler) Edit(c *gin.Context) {
	space, task, actor, valid := h.boundary.request(c)
	if !valid {
		return
	}
	var request service.EditContentRequest
	if !h.bind(c, &request) {
		return
	}
	result, err := h.boundary.service.Edit(c.Request.Context(), space, task, actor, c.Param("content_id"), request)
	h.boundary.respond(c, result, err)
}

func (h *ContentCommandHandler) Restore(c *gin.Context) {
	space, task, actor, valid := h.boundary.request(c)
	if !valid {
		return
	}
	var request service.RestoreContentRequest
	if !h.bind(c, &request) {
		return
	}
	result, err := h.boundary.service.Restore(c.Request.Context(), space, task, actor, c.Param("content_id"), request)
	h.boundary.respond(c, result, err)
}

func (h *ContentCommandHandler) Refine(c *gin.Context) {
	space, task, actor, valid := h.boundary.request(c)
	if !valid {
		return
	}
	var request service.RefineContentRequest
	if !h.bind(c, &request) {
		return
	}
	result, err := h.boundary.service.QueueRefine(c.Request.Context(), space, task, actor, c.Param("content_id"), request)
	h.boundary.respond(c, result, err)
}

func (h *ContentCommandHandler) Cancel(c *gin.Context) {
	space, task, actor, valid := h.boundary.request(c)
	if !valid {
		return
	}
	ctx, contentID, generationID := c.Request.Context(), c.Param("content_id"), c.Param("generation_id")
	if err := h.boundary.service.CancelGeneration(ctx, space, task, actor, contentID, generationID); err != nil {
		h.boundary.respond(c, nil, err)
		return
	}
	result, err := h.boundary.service.Generation(ctx, space, task, actor, contentID, generationID)
	h.boundary.respond(c, result, err)
}

func (h *ContentCommandHandler) Apply(c *gin.Context) {
	space, task, actor, valid := h.boundary.request(c)
	if !valid {
		return
	}
	var request service.ContentBaseline
	if !h.bind(c, &request) {
		return
	}
	result, err := h.boundary.service.ApplyCandidate(c.Request.Context(), space, task, actor,
		c.Param("content_id"), c.Param("generation_id"), request)
	h.boundary.respond(c, result, err)
}
