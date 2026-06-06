package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/michondr/audiobookshelf-hardcover-sync/internal/abs"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/config"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/db"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/email"
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

	var mailer *email.Mailer
	if cfg.SMTPEnabled() {
		mailer = email.New(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPFrom, cfg.SMTPTo)
		log.Info("SMTP notifications enabled", "host", cfg.SMTPHost, "to", cfg.SMTPTo)
	}

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

		matchStats, err := syncService.MatchUnmatched(ctx)
		if err != nil {
			log.Error("cron: match unmatched", "err", err)
		}

		if err := syncService.RefreshHCProgress(ctx); err != nil {
			log.Error("cron: refresh HC progress", "err", err)
		}

		var syncedTitles []string
		if on, err := database.GetBoolSetting(ctx, db.SettingAutoSyncProgress); err != nil {
			log.Error("cron: read auto-sync setting", "err", err)
		} else if on {
			if titles, err := syncService.AutoSyncOutOfSync(ctx); err != nil {
				log.Error("cron: auto-sync progress", "err", err)
			} else {
				syncedTitles = titles
				log.Info("cron: auto-synced progress", "count", len(titles))
			}
		}

		log.Info("cron: refresh complete")

		if mailer != nil {
			notify, err := database.GetBoolSetting(ctx, db.SettingEmailNotify)
			if err != nil {
				log.Error("cron: read email-notify setting", "err", err)
			}
			if notify && (len(matchStats.Matched) > 0 || len(syncedTitles) > 0 || matchStats.Candidates > 0 || matchStats.NotFound > 0) {
				subject, body := buildSyncEmail(matchStats, syncedTitles)
				if err := mailer.Send(subject, body); err != nil {
					log.Error("cron: send email", "err", err)
				} else {
					log.Info("cron: email sent")
				}
			}
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

	handler := web.NewServer(database, absClient, hcClient, syncService, log, nextSync, cfg.SMTPEnabled(), mailer, cfg.SMTPTo)

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

func buildSyncEmail(ms syncsvc.MatchStats, synced []string) (subject, body string) {
	var sb strings.Builder
	sb.WriteString("Sync completed at " + time.Now().Format("2006-01-02 15:04") + ".\n")

	if len(synced) > 0 {
		fmt.Fprintf(&sb, "\nProgress synced to Hardcover (%d):\n", len(synced))
		for _, t := range synced {
			fmt.Fprintf(&sb, "  - %s\n", t)
		}
	}

	if len(ms.Matched) > 0 {
		fmt.Fprintf(&sb, "\nNewly matched to Hardcover (%d):\n", len(ms.Matched))
		for _, t := range ms.Matched {
			fmt.Fprintf(&sb, "  - %s\n", t)
		}
	}

	if ms.Candidates > 0 {
		fmt.Fprintf(&sb, "\nBooks with multiple Hardcover candidates (pick manually): %d\n", ms.Candidates)
	}

	if ms.NotFound > 0 {
		fmt.Fprintf(&sb, "\nBooks not found on Hardcover: %d\n", ms.NotFound)
	}

	totalChanged := len(synced) + len(ms.Matched)
	subject = fmt.Sprintf("ABS→Hardcover sync: %d book(s) updated", totalChanged)
	body = sb.String()
	return
}
