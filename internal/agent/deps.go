package agent

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

// summaryDeps holds the dependencies required by summary-related tools.
// Set via SetSummaryDeps at API startup; read by tool factories via GetSummaryDeps.
type summaryDeps struct {
	imDB       *gorm.DB
	octoClient *service.OctoSearchBatchClient
	cfg        config.Config
}

var (
	depsMu   sync.RWMutex
	summDeps summaryDeps
	depsSet  bool
)

// SetSummaryDeps injects dependencies for summary tools.
// Call once at API startup before any summary tool is invoked.
func SetSummaryDeps(imDB *gorm.DB, octoClient *service.OctoSearchBatchClient, cfg config.Config) {
	depsMu.Lock()
	defer depsMu.Unlock()
	summDeps = summaryDeps{
		imDB:       imDB,
		octoClient: octoClient,
		cfg:        cfg,
	}
	depsSet = true
}

// GetSummaryDeps returns the injected dependencies.
// Panics if SetSummaryDeps has not been called yet.
func GetSummaryDeps() (imDB *gorm.DB, octoClient *service.OctoSearchBatchClient, cfg config.Config) {
	depsMu.RLock()
	defer depsMu.RUnlock()
	if !depsSet {
		panic("summary deps not set: call SetSummaryDeps before using summary tools")
	}
	return summDeps.imDB, summDeps.octoClient, summDeps.cfg
}

// ChannelInfo is an alias for pipeline.ChannelInfo for convenience in tool handlers.
type ChannelInfo = pipeline.ChannelInfo
