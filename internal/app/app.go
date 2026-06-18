package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/controlplane"
	"insurance-benefit-agent-go/internal/jobmgr"
	"insurance-benefit-agent-go/internal/payers"
	"insurance-benefit-agent-go/internal/payers/deltadentalins"
	"insurance-benefit-agent-go/internal/triggerapi"
	"insurance-benefit-agent-go/internal/updater"
)

type App struct {
	cfg     *config.Config
	manager *jobmgr.Manager
	api     *triggerapi.Server
	updater *updater.Service
}

func New(cfg *config.Config) (*App, error) {
	control := controlplane.NewClient(cfg)
	registry := payers.NewRegistry()
	registry.Register(deltadentalins.NewAdapter(control))

	manager := jobmgr.New(cfg, control, registry)
	if cfg.SnapshotPath != "" {
		if err := manager.SeedSnapshot(cfg.SnapshotPath); err != nil {
			return nil, fmt.Errorf("seed snapshot: %w", err)
		}
	}
	app := &App{cfg: cfg, manager: manager}
	if cfg.API.Enabled {
		updateSvc, err := updater.New(cfg.Updates, cfg.Bootstrap.Patcon.URL, cfg.Bootstrap.Patcon.Token, cfg.Path(), os.Args[1:])
		if err != nil {
			log.Printf("updater unavailable: %v", err)
		}
		app.updater = updateSvc
		app.api = triggerapi.New(cfg.API, cfg.OfficeKey, manager, updateSvc)
	}
	return app, nil
}

func (a *App) Run(ctx context.Context, runOnce bool) error {
	if runOnce {
		return a.manager.Run(ctx, runOnce)
	}
	if a.api != nil {
		go a.manager.StartQueueChecker(ctx)
		go a.startUpdateChecker(ctx)
		return a.api.Run(ctx)
	}
	return a.manager.Run(ctx, false)
}

func (a *App) startUpdateChecker(ctx context.Context) {
	if a == nil || a.updater == nil || a.cfg == nil || !a.cfg.Updates.Enabled {
		return
	}
	minutes := a.cfg.Updates.CheckIntervalMinutes
	if minutes <= 0 {
		minutes = 60
	}
	interval := time.Duration(minutes) * time.Minute

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.manager.Status().Running {
				log.Printf("update check skipped: agent busy")
				continue
			}
			check := a.updater.Check()
			if !check.UpdateAvailable {
				log.Printf("update check complete: %s", check.Reason)
				continue
			}
			if a.manager.Status().Running {
				log.Printf("update apply skipped: agent became busy")
				continue
			}
			version := ""
			channel := ""
			if check.Manifest != nil {
				version = check.Manifest.Version
				channel = check.Manifest.Channel
			}
			log.Printf("update available version=%s channel=%s; applying", version, channel)
			result, err := a.updater.Apply()
			if err != nil {
				log.Printf("update apply failed: %v", err)
				continue
			}
			if result.Started {
				log.Printf("update apply started; exiting for updater")
				os.Exit(0)
			}
			log.Printf("update apply complete: %s", result.Message)
		}
	}
}
