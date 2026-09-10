package handler

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
)

// Fast rejection before streaming/model execution. The transaction fence must
// still recheck this after execution to cover in-flight migration races.
func allowLegacyContentCommand(c *gin.Context, task model.SummaryTask) bool {
	if service.ContentProtocolRequired(task) {
		bizErr(c, service.ErrContentProtocolRequired)
		return false
	}
	return true
}
