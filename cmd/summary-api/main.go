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
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/ws"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/db"
	appredis "github.com/Mininglamp-OSS/octo-smart-summary/internal/redis"
)

func main() {
	cfg := config.Load()

	// Init DB
	summaryDB, err := db.New(cfg.MySQLDSN)
	if err != nil {
		log.Fatalf("[main] connect summary DB: %v", err)
	}

	// Init IM DB (for member candidates)
	imDB, err := db.New(cfg.IMMySQLDSN)
	if err != nil {
		log.Printf("[main] connect IM DB (non-fatal): %v", err)
		imDB = nil
	}

	// Init Redis
	rdb := appredis.New(cfg.RedisAddr, cfg.RedisDB)

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

	// Public API server
	publicRouter := router.SetupPublic(summaryDB, imDB, hub, rdb, cfg.WorkerTriggerURL)
	publicSrv := &http.Server{
		Addr:    ":" + cfg.APIPort,
		Handler: publicRouter,
	}

	// Internal callback server (Docker network accessible for worker callbacks)
	internalRouter, _ := router.SetupInternal(hub)
	internalSrv := &http.Server{
		Addr:    ":" + cfg.APIInternalPort, // 0.0.0.0 so worker container can reach via Docker network
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

	if rdb != nil {
		rdb.Close()
	}

	log.Println("[api] exited")
}
