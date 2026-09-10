package handler

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ContentReadHandler is the additive, allowlisted compatibility endpoint.
// It neither replaces legacy DTOs nor enables the new write protocol.
type ContentReadHandler struct {
	service          *service.ContentService
	spaces           map[string]bool
	executionService *service.ContentService
	executionSpaces  map[string]bool
}

func (h *ContentReadHandler) WithExecution(spaces string, authorizer service.GenerationSourceAuthorizer, maxDays int) *ContentReadHandler {
	h.executionSpaces = service.ParseContentReadSpaces(spaces)
	h.executionService = h.service.WithExecution(authorizer, maxDays)
	for space := range h.executionSpaces {
		h.spaces[space] = true
	}
	return h
}

func NewContentReadHandler(db *gorm.DB, enabledSpaces string) *ContentReadHandler {
	return &ContentReadHandler{service: service.NewContentService(db), spaces: service.ParseContentReadSpaces(enabledSpaces)}
}

func (h *ContentReadHandler) request(c *gin.Context) (string, int64, string, bool) {
	spaceID, actorID := middleware.GetSpaceID(c), middleware.GetUserID(c)
	if actorID == "" {
		c.JSON(http.StatusUnauthorized, apiResponse{Code: 4010, Message: "authentication required"})
		return "", 0, "", false
	}
	if !h.spaces[spaceID] {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "not found", Detail: "content_contract_not_enabled"})
		return "", 0, "", false
	}
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || taskID <= 0 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid summary id"})
		return "", 0, "", false
	}
	return spaceID, taskID, actorID, true
}

func (h *ContentReadHandler) Catalog(c *gin.Context) {
	spaceID, taskID, actorID, valid := h.request(c)
	if !valid {
		return
	}
	reader := h.service
	if h.executionSpaces[spaceID] {
		reader = h.executionService
	}
	result, err := reader.Catalog(c.Request.Context(), spaceID, taskID, actorID)
	h.respond(c, result, err)
}

func (h *ContentReadHandler) Versions(c *gin.Context) {
	spaceID, taskID, actorID, valid := h.request(c)
	if !valid {
		return
	}
	limit := 20
	if value, present := c.GetQuery("limit"); present {
		var err error
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			h.respond(c, nil, &service.ContentError{Code: "invalid_page_limit", HTTPStatus: 400})
			return
		}
	}
	result, err := h.service.Versions(c.Request.Context(), spaceID, taskID, actorID,
		c.Param("content_id"), c.Query("cursor"), limit)
	h.respond(c, result, err)
}

func (h *ContentReadHandler) Version(c *gin.Context) {
	spaceID, taskID, actorID, valid := h.request(c)
	if !valid {
		return
	}
	result, err := h.service.Version(c.Request.Context(), spaceID, taskID, actorID,
		c.Param("content_id"), c.Param("version_id"))
	h.respond(c, result, err)
}

func (h *ContentReadHandler) Generation(c *gin.Context) {
	spaceID, taskID, actorID, valid := h.request(c)
	if !valid {
		return
	}
	result, err := h.service.Generation(c.Request.Context(), spaceID, taskID, actorID,
		c.Param("content_id"), c.Param("generation_id"))
	h.respond(c, result, err)
}

func (h *ContentReadHandler) respond(c *gin.Context, value any, err error) {
	if err == nil {
		ok(c, value)
		return
	}
	var ce *service.ContentError
	if errors.As(err, &ce) {
		c.JSON(ce.HTTPStatus, apiResponse{Code: ce.HTTPStatus * 100, Message: ce.Code, Detail: ce.Code})
		return
	}
	log.Printf("[summary-content] operation failed: %v", err)
	c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error", Detail: "internal_error"})
}
