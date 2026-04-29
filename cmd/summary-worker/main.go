package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/router"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/ws"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/db"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/worker"
)

func main() {
	cfg := config.Load()

	// Init summary DB
	summaryDB, err := db.New(cfg.MySQLDSN)
	if err != nil {
		log.Fatalf("[worker] connect summary DB: %v", err)
	}

	// Init IM DB (read-only, for message fetching)
	imDB, err := db.New(cfg.IMMySQLDSN)
	if err != nil {
		log.Fatalf("[worker] connect IM DB: %v", err)
	}

	// Init LLM client
	llm := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken)

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

	// Start processor (polling loop)
	proc := worker.NewProcessor(summaryDB, imDB, pool, llm, cfg)
	go proc.Run()

	// Start scheduler (cron jobs)
	workerTriggerURL := "http://127.0.0.1:" + cfg.WorkerInternalPort + "/internal/worker-trigger"
	// Worker internal trigger server listens on all interfaces so API container can reach it
	workerTriggerListenAddr := cfg.WorkerListenAllInterfaces
	cronSched := worker.StartScheduler(summaryDB, cfg.WorkerMaxRetry, workerTriggerURL)

	// Start internal HTTP server for worker-trigger
	hub := ws.NewHub(summaryDB)
	internalRouter, intH := router.SetupInternal(hub)
	intH.SetTriggerCh(proc.TriggerCh())
	internalSrv := &http.Server{
		Addr:    workerTriggerListenAddr + ":" + cfg.WorkerInternalPort,
		Handler: internalRouter,
	}
	go func() {
		log.Printf("[worker] internal server listening on %s:%s", workerTriggerListenAddr, cfg.WorkerInternalPort)
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
