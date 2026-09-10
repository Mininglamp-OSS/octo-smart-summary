package handler

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ContentCommandHandler is the coordinated command adapter. Production mounts
// only the supported single-person pilot, behind the exact execution allowlist.
// SUMMARY_CONTENT_READ_SPACES must never enable these commands.
type ContentCommandHandler struct {
	boundary   *ContentReadHandler
	singleOnly bool
}

func (h *ContentCommandHandler) WithExecution(authorizer service.GenerationSourceAuthorizer, maxDays int) *ContentCommandHandler {
	h.boundary.service = h.boundary.service.WithExecution(authorizer, maxDays)
	h.singleOnly = true
	return h
}

func (h *ContentCommandHandler) request(c *gin.Context) (string, int64, string, bool) {
	space, task, actor, valid := h.boundary.request(c)
	if valid && h.singleOnly {
		if err := h.boundary.service.CheckExecutionTarget(c.Request.Context(), space, task, actor, c.Param("content_id")); err != nil {
			h.boundary.respond(c, nil, err)
			return "", 0, "", false
		}
	}
	return space, task, actor, valid
}

func NewContentCommandHandler(db *gorm.DB, writeSpaces string) *ContentCommandHandler {
	return &ContentCommandHandler{boundary: NewContentReadHandler(db, writeSpaces)}
}

func (h *ContentCommandHandler) bind(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4*1024*1024)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		h.boundary.respond(c, nil, &service.ContentError{Code: "invalid_request", HTTPStatus: 400})
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		h.boundary.respond(c, nil, &service.ContentError{Code: "invalid_request", HTTPStatus: 400})
		return false
	}
	return true
}

func (h *ContentCommandHandler) Edit(c *gin.Context) {
	space, task, actor, valid := h.request(c)
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
	space, task, actor, valid := h.request(c)
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
	space, task, actor, valid := h.request(c)
	if !valid {
		return
	}
	var request service.RefineContentRequest
	if !h.bind(c, &request) {
		return
	}
	result, err := h.boundary.service.QueueRefine(c.Request.Context(), space, task, actor, c.Param("content_id"), request)
	h.accepted(c, result, err)
}

func (h *ContentCommandHandler) Cancel(c *gin.Context) {
	space, task, actor, valid := h.request(c)
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
	space, task, actor, valid := h.request(c)
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

func (h *ContentCommandHandler) Configuration(c *gin.Context) {
	space, task, actor, valid := h.request(c)
	if !valid {
		return
	}
	result, err := h.boundary.service.GenerationConfiguration(c.Request.Context(), space, task, actor, c.Param("content_id"))
	h.boundary.respond(c, result, err)
}

func (h *ContentCommandHandler) SaveConfiguration(c *gin.Context) {
	space, task, actor, valid := h.request(c)
	if !valid {
		return
	}
	var request struct {
		service.SaveGenerationConfigurationRequest
		Revision *int64 `json:"expected_config_revision"`
	}
	if !h.bind(c, &request) {
		return
	}
	if request.Revision == nil {
		h.boundary.respond(c, nil, &service.ContentError{Code: "configuration_baseline_required", HTTPStatus: 400})
		return
	}
	request.ExpectedConfigRevision = *request.Revision
	result, err := h.boundary.service.SaveGenerationConfiguration(c.Request.Context(), space, task, actor, c.Param("content_id"), request.SaveGenerationConfigurationRequest)
	if result.Generation != nil {
		h.accepted(c, result, err)
	} else {
		h.boundary.respond(c, result, err)
	}
}

func (h *ContentCommandHandler) Regenerate(c *gin.Context) {
	space, task, actor, valid := h.request(c)
	if !valid {
		return
	}
	var request service.RegenerateContentRequest
	if !h.bind(c, &request) {
		return
	}
	result, err := h.boundary.service.QueueRegenerate(c.Request.Context(), space, task, actor, c.Param("content_id"), request)
	h.accepted(c, result, err)
}

func (h *ContentCommandHandler) accepted(c *gin.Context, value any, err error) {
	if err != nil {
		h.boundary.respond(c, nil, err)
		return
	}
	c.JSON(http.StatusAccepted, apiResponse{Code: 0, Message: "ok", Data: value})
}
