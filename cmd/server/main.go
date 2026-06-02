package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/michondr/audiobookshelf-hardcover-sync/internal/abs"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/config"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/db"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/hardcover"
	syncsvc "github.com/michondr/audiobookshelf-hardcover-sync/internal/sync"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Error("db open", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	absClient := abs.New(cfg.ABSUrl, cfg.ABSToken)
	hcClient := hardcover.New(cfg.HardcoverToken)
	syncService := syncsvc.New(database, absClient, hcClient, log)

	loc, err := time.LoadLocation(cfg.CronTimezone)
	if err != nil {
		log.Error("load timezone", "tz", cfg.CronTimezone, "err", err)
		os.Exit(1)
	}

	c := cron.New(cron.WithLocation(loc))
	entryID, err := c.AddFunc(cfg.CronSchedule, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		log.Info("cron: starting refresh")
		if err := syncService.RefreshFromABS(ctx); err != nil {
			log.Error("cron: refresh from ABS", "err", err)
			return
		}
		if err := syncService.MatchUnmatched(ctx); err != nil {
			log.Error("cron: match unmatched", "err", err)
		}
		if err := syncService.RefreshHCProgress(ctx); err != nil {
			log.Error("cron: refresh HC progress", "err", err)
		}
		log.Info("cron: refresh complete")
	})
	if err != nil {
		log.Error("cron schedule", "schedule", cfg.CronSchedule, "err", err)
		os.Exit(1)
	}

	c.Start()
	defer c.Stop()

	nextSync := func() time.Time {
		return c.Entry(entryID).Next
	}

	handler := web.NewServer(database, absClient, hcClient, syncService, log, nextSync)

	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 90 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Info("starting server", "addr", addr, "cron", cfg.CronSchedule, "tz", cfg.CronTimezone)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
