package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/router"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/ws"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/db"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/notify"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timing"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/worker"
	"gorm.io/gorm"
)

// summaryNotificationRobotID 是「总结助手」系统 bot 的固定 UID，与 octo-server 的
// pkg/space.SummaryNotificationBotUID 一致（单一真源在 server 侧）。summary 与 server
// 共享同一 MySQL `im` 库，这里硬编码该常量字符串用于从 robot 表查 bot_token。
const summaryNotificationRobotID = "summary_notification"

// resolveSummaryBotToken 解析通知 bot 的 bot_token（OCT-5 / 方案 D：共享 DB）。
//
// 优先级：
//  1. 若 env SUMMARY_BOT_TOKEN（envToken）非空则直接用之（向后兼容旧部署）；
//  2. 为空才从共享 IM 库查 robot 表。server 首启动时会自动生成强随机 token 写入
//     该表（见 octo-server modules/robot/api.go ensureSummaryBotToken）。
//
// 启动顺序竞态修复：本函数现在作为 HTTPDeliverer 的 lazy tokenFn 使用，在每次投递前
// 被调用（带非空值缓存），不再在启动时用其返回值决定 disable。拿到空（未生成 /
// 表无行）返回 error，让本次投递失败走既有 best-effort，下次再查；不 panic。
//
// imDB 为 nil 防护：imDB==nil 时直接返回空 + error，不解引空指针 panic。
func resolveSummaryBotToken(envToken string, imDB *gorm.DB) (string, error) {
	if envToken != "" {
		return envToken, nil
	}
	if imDB == nil {
		return "", fmt.Errorf("resolveSummaryBotToken: imDB is nil and env SUMMARY_BOT_TOKEN empty")
	}
	var token string
	// 参照 main.go SetUserNameResolver 里的 imDB.Raw(...).Scan(...) 用法。
	if err := imDB.Raw("SELECT bot_token FROM robot WHERE robot_id = ? LIMIT 1", summaryNotificationRobotID).Scan(&token).Error; err != nil {
		return "", fmt.Errorf("resolveSummaryBotToken: query robot.bot_token: %w", err)
	}
	return token, nil
}

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

	// Optional override of the per-stage timing log path; defaults to
	// timing.DefaultLogPath (/var/log/smart-summary/timing.log).
	if p := os.Getenv("TIMING_LOG_PATH"); p != "" {
		timing.SetLogPath(p)
	}
	// Optional override of the per-run LLM summary report path; defaults to
	// timing.DefaultReportPath (/var/log/smart-summary/summary-report.log).
	if p := os.Getenv("SUMMARY_REPORT_PATH"); p != "" {
		timing.SetReportPath(p)
	}
	config.ValidateRequired(map[string]string{
		"MYSQL_DSN":               cfg.MySQLDSN,
		"IM_MYSQL_DSN":            cfg.IMMySQLDSN,
		"LLM_API_URL":             cfg.LLMApiURL,
		"LLM_API_KEY":             cfg.LLMApiKey,
		"LLM_MODEL":               cfg.LLMModel,
		"WORKER_API_CALLBACK_URL": cfg.WorkerCallbackURL,
	})

	// Init summary DB
	summaryDB, err := db.New(cfg.MySQLDSN)
	if err != nil {
		log.Fatalf("[worker] connect summary DB: %v", err)
	}

	// Run database migrations
	sqlDB, err := summaryDB.DB()
	if err != nil {
		log.Fatalf("[worker] get raw db: %v", err)
	}
	n, err := db.RunMigrations(sqlDB)
	if err != nil {
		log.Fatalf("[worker] migration failed: %v", err)
	}
	if n > 0 {
		log.Printf("[worker] applied %d migration(s)", n)
	}

	// Init IM DB (read-only, for message fetching)
	imDB, err := db.New(cfg.IMMySQLDSN)
	if err != nil {
		log.Fatalf("[worker] connect IM DB: %v", err)
	}

	// Init LLM client
	llm := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, cfg.ToolCallTimeout)

	// Set up user/source name resolvers (same as API process)
	service.SetUserNameResolver(func(uid string) string {
		var name string
		imDB.Raw("SELECT name FROM `user` WHERE uid = ? LIMIT 1", uid).Scan(&name)
		if name != "" {
			return name
		}
		return uid
	})

	// Start worker pool
	pool := worker.NewWorkerPool(cfg.WorkerMaxConcurrent)

	// Build the terminal-state IM-bot notifier (OCT-4). Disabled unless
	// SUMMARY_NOTIFY_ENABLED=true; a nil deliverer / disabled config makes
	// OnTaskTerminal a no-op, so it is always safe to wire.
	var notifier *notify.Notifier
	if cfg.NotifyEnabled {
		// OCT-5 / 方案 D（共享 DB）+ 启动顺序竞态修复：bot_token 不再靠 env 写死/注入，
		// 也不再在启动时一次性固化。server 首启动时自动生成强随机 token 写入共享 IM 库的
		// robot 表；summary 这边优先用 env SUMMARY_BOT_TOKEN（向后兼容），为空才从 IM 库查。
		//
		// 关键：OCTO_API_URL 非空时总是装配 notifier，不再因启动时 token 空而 disable。
		// token 改为 HTTPDeliverer 首次投递时 lazy 解析 + 缓存；若 worker 先于 server 起，
		// 启动时拿不到 token 也不再永久禁用，server 后写了 token 后后续投递自动恢复。
		if cfg.OctoAPIURL == "" {
			log.Printf("[worker] SUMMARY_NOTIFY_ENABLED=true but OCTO_API_URL missing; notifications disabled")
		} else {
			tokenFn := func() (string, error) {
				return resolveSummaryBotToken(cfg.SummaryBotToken, imDB)
			}
			deliverer := notify.NewHTTPDelivererWithTokenFunc(cfg.OctoAPIURL, tokenFn)
			// imDB（共享 IM 库）注入用于在通知文本中解析空间名（只读查 space 表）；
			// imDB 为 nil / 查不到时 buildText 优雅降级回不带空间名的旧文案。
			notifier = notify.New(summaryDB, imDB, deliverer, notify.Config{
				Enabled:     true,
				WebBaseURL:  cfg.SummaryWebBaseURL,
				MaxAttempts: cfg.MaxNotifyAttempts,
				QuietStart:  cfg.NotifyQuietStart,
				QuietEnd:    cfg.NotifyQuietEnd,
			})
			log.Printf("[worker] terminal-state notifications ENABLED (bot token resolved lazily on first delivery; survives server-started-after-worker race)")
		}
	}

	// Start processor (polling loop)
	proc := worker.NewProcessor(summaryDB, imDB, pool, llm, cfg)
	proc.SetNotifier(notifier)
	go proc.Run()

	// Start scheduler (cron jobs)
	cronSched := worker.StartScheduler(summaryDB, imDB, cfg.WorkerMaxRetry, cfg.WorkerTriggerURL, cfg.ScheduleMaxWindowDays, cfg.FeatureTeamSchedule, notifier)

	// Start internal HTTP server for worker-trigger
	hub := ws.NewHub(summaryDB)
	internalRouter, intH := router.SetupInternal(hub)
	intH.SetTriggerCh(proc.TriggerCh())
	internalSrv := &http.Server{
		Addr:    cfg.WorkerListenAllInterfaces + ":" + cfg.WorkerInternalPort,
		Handler: internalRouter,
	}
	go func() {
		log.Printf("[worker] internal server listening on %s:%s", cfg.WorkerListenAllInterfaces, cfg.WorkerInternalPort)
		if err := internalSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[worker] internal server error: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	log.Println("[worker] shutting down...")

	proc.Stop()
	pool.Drain()
	cronSched.Stop()

	log.Println("[worker] exited")
}
