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
	syncsvc "github.com/michondr/audiobookshelf-hardcover-sync/internal/sync"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/hardcover"
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

	// Cron scheduler
	loc, err := time.LoadLocation(cfg.CronTimezone)
	if err != nil {
		log.Error("load timezone", "tz", cfg.CronTimezone, "err", err)
		os.Exit(1)
	}

	c := cron.New(cron.WithLocation(loc))

	entryID, err := c.AddFunc(cfg.CronSchedule, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		log.Info("cron: starting sync")
		// RefreshFromABS also runs re-read detection internally.
		if err := syncService.RefreshFromABS(ctx); err != nil {
			log.Error("cron: refresh from ABS", "err", err)
		}
		n, err := syncService.SyncAllProgress(ctx)
		if err != nil {
			log.Error("cron: sync progress", "err", err)
		} else {
			log.Info("cron: sync complete", "books_synced", n)
		}
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

	handler := web.NewServer(database, absClient, syncService, log, nextSync)

	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 90 * time.Second, // generous for cover proxy streaming
		IdleTimeout:  120 * time.Second,
	}

	log.Info("starting server", "addr", addr, "cron", cfg.CronSchedule, "tz", cfg.CronTimezone)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
