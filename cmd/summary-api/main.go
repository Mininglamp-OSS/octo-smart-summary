package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/router"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/ws"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/auth"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/db"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func main() {
	cfg := config.Load()

	// Apply config to pipeline package-level variables
	if cfg.MaxSafetyLimit > 0 {
		pipeline.MaxSafetyLimit = cfg.MaxSafetyLimit
	}
	if cfg.DefaultTimeRangeDays > 0 {
		pipeline.DefaultTimeRangeDays = cfg.DefaultTimeRangeDays
	}
	// EnableIntentShortcut defaults to true, so we always apply it
	pipeline.EnableIntentShortcut = cfg.EnableIntentShortcut

	config.ValidateRequired(map[string]string{
		"MYSQL_DSN":          cfg.MySQLDSN,
		"IM_MYSQL_DSN":       cfg.IMMySQLDSN,
		"OCTO_API_URL":       cfg.OctoAPIURL,
		"WORKER_TRIGGER_URL": cfg.WorkerTriggerURL,
	})

	// Init DB
	summaryDB, err := db.New(cfg.MySQLDSN)
	if err != nil {
		log.Fatalf("[main] connect summary DB: %v", err)
	}

	// Run database migrations
	sqlDB, err := summaryDB.DB()
	if err != nil {
		log.Fatalf("[main] get raw db: %v", err)
	}
	n, err := db.RunMigrations(sqlDB)
	if err != nil {
		log.Fatalf("[main] migration failed: %v", err)
	}
	if n > 0 {
		log.Printf("[main] applied %d migration(s)", n)
	}

	// Init IM DB (for member candidates)
	imDB, err := db.New(cfg.IMMySQLDSN)
	if err != nil {
		log.Printf("[main] connect IM DB (non-fatal): %v", err)
		imDB = nil
	}

	// Init auth resolver
	httpResolver := auth.NewHTTPTokenResolver(cfg.OctoAPIURL)
	authResolver := auth.NewCachedResolver(httpResolver, 30*time.Second, 10000)

	// Init WebSocket hub
	hub := ws.NewHub(summaryDB)

	// Inject IM DB resolvers
	if imDB != nil {
		service.SetSourceNameResolver(func(sourceID string) string {
			var name string
			imDB.Raw("SELECT name FROM `group` WHERE group_no = ? LIMIT 1", sourceID).Scan(&name)
			if name != "" {
				return name
			}
			if len(sourceID) > 8 {
				return "来源-" + sourceID[:8]
			}
			return "来源-" + sourceID
		})
		service.SetUserNameResolver(func(uid string) string {
			var name string
			imDB.Raw("SELECT name FROM `user` WHERE uid = ? LIMIT 1", uid).Scan(&name)
			if name != "" {
				return name
			}
			return uid
		})
	}

	// Public API server. Refine is optional: existing API deployments can still start
	// without LLM envs, while /summaries/:id/refine returns 503 until configured.
	var refineLLM *service.LLMClient
	if cfg.LLMApiURL != "" && cfg.LLMApiKey != "" && cfg.LLMModel != "" {
		refineLLM = service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, cfg.ToolCallTimeout)
	}
	publicRouter := router.SetupPublic(summaryDB, imDB, hub, authResolver, cfg.WorkerTriggerURL, cfg.CandidateQueryLimit, cfg.FeatureTeamSchedule, cfg.SummaryCustomTemplateLimit, refineLLM)
	publicSrv := &http.Server{
		Addr:    ":" + cfg.APIPort,
		Handler: publicRouter,
	}

	// Internal callback server (Docker network accessible for worker callbacks)
	internalRouter, _ := router.SetupInternal(hub)
	internalSrv := &http.Server{
		Addr:    ":" + cfg.APIInternalPort,
		Handler: internalRouter,
	}

	// Start servers
	go func() {
		log.Printf("[api] public server listening on :%s", cfg.APIPort)
		if err := publicSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[api] public server: %v", err)
		}
	}()

	go func() {
		log.Printf("[api] internal server listening on :%s", cfg.APIInternalPort)
		if err := internalSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[api] internal server: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	log.Println("[api] shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	publicSrv.Shutdown(ctx)
	internalSrv.Shutdown(ctx)

	log.Println("[api] exited")
}
