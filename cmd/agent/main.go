package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"insurance-benefit-agent-go/internal/app"
	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/logging"
	"insurance-benefit-agent-go/internal/version"
)

func main() {
	configPath   := flag.String("config", "agent.config.json", "Path to optional config file for non-required settings")
	runOnce      := flag.Bool("run-once", false, "Run once and exit")
	showVersion  := flag.Bool("version", false, "Print version and exit")
	snapshotPath := flag.String("snapshot", "", "Path to a local snapshot JSON file; bypasses PatCon bootstrap when set")
	addDays      := flag.Int("add-days", 1, "Number of days ahead to fetch appointments when using --run-once")

	flagOfficeKey   := flag.String("office-key", "", "Office key (required)")
	flagPatconURL   := flag.String("patcon-url", "", "Patcon bootstrap URL (required without --snapshot)")
	flagPatconToken := flag.String("patcon-token", "", "Patcon bearer token (required without --snapshot)")
	flagPort        := flag.String("port", "", "API listen port, e.g. 8080 (required)")
	flagAPIToken    := flag.String("api-token", "", "API bearer token (defaults to patcon-token if omitted)")
	flag.Parse()

	if *showVersion {
		info := version.Get()
		fmt.Printf("agent version=%s commit=%s builtAt=%s goos=%s goarch=%s\n",
			info.Version, info.Commit, info.BuiltAt, info.GoOS, info.GoArch)
		return
	}

	// Required flags — PatCon URL/token are only required when not using --snapshot.
	var missing []string
	if *flagOfficeKey == "" {
		missing = append(missing, "--office-key")
	}
	usingSnapshot := *snapshotPath != ""
	if !usingSnapshot {
		if *flagPatconURL == "" {
			missing = append(missing, "--patcon-url")
		}
		if *flagPatconToken == "" {
			missing = append(missing, "--patcon-token")
		}
	}
	if *flagPort == "" {
		missing = append(missing, "--port")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "error: required flags missing: %s\n\n", strings.Join(missing, ", "))
		flag.Usage()
		os.Exit(1)
	}

	// Config file is optional — used for non-required settings (sweep, cache,
	// pdf, updates, testing). Fall back to built-in defaults if absent.
	cfg, err := config.Load(*configPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Fatalf("load config: %v", err)
		}
		cfg, err = config.Default(*configPath)
		if err != nil {
			log.Fatalf("default config: %v", err)
		}
	}

	// Always apply the required flags — they win over anything in the config file.
	cfg.OfficeKey = *flagOfficeKey
	cfg.Bootstrap.Patcon.URL = *flagPatconURL
	cfg.Bootstrap.Patcon.Token = *flagPatconToken
	cfg.SnapshotPath = *snapshotPath
	cfg.RunOnceAddDays = *addDays
	cfg.API.Enabled = true
	cfg.API.ListenAddr = "127.0.0.1:" + *flagPort
	if *flagAPIToken != "" {
		cfg.API.BearerToken = stripBearer(*flagAPIToken)
	} else if cfg.API.BearerToken == "" {
		cfg.API.BearerToken = stripBearer(*flagPatconToken)
	}

	logFile, logFilePath, err := configureLogging(cfg)
	if err != nil {
		log.Fatalf("configure logging: %v", err)
	}
	defer logFile.Close()
	log.Printf("log file: %s", logFilePath)
	info := version.Get()
	log.Printf("agent version=%s commit=%s builtAt=%s goos=%s goarch=%s",
		info.Version, info.Commit, info.BuiltAt, info.GoOS, info.GoArch)
	pruneOldLogs(filepath.Dir(logFilePath), 5*24*time.Hour)
	eventLogPath, err := logging.Configure(filepath.Join(filepath.Dir(logFilePath)), cfg.OfficeKey)
	if err != nil {
		log.Fatalf("configure structured logging: %v", err)
	}
	logging.Info("agent", "agent.logging.configured", "structured event logging configured", map[string]any{
		"logFile":      logFilePath,
		"eventLogFile": eventLogPath,
	})

	agentApp, err := app.New(cfg)
	if err != nil {
		logging.Critical("agent", "agent.startup.create_app_failed", "failed to create app", map[string]any{
			"error": err.Error(),
		})
		log.Fatalf("create app: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Run owns the full lifecycle: bootstrap, heartbeat, snapshot, and sweeps.
	if err := agentApp.Run(ctx, *runOnce); err != nil {
		logging.Critical("agent", "agent.run.failed", "agent run failed", map[string]any{
			"error": err.Error(),
		})
		log.Fatalf("run app: %v", err)
	}
}

func stripBearer(tok string) string {
	if len(tok) > 7 && strings.EqualFold(tok[:7], "bearer ") {
		return strings.TrimSpace(tok[7:])
	}
	return tok
}

func pruneOldLogs(logDir string, maxAge time.Duration) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		log.Printf("prune logs: read dir: %v", err)
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(logDir, e.Name())
		if removeErr := os.Remove(path); removeErr != nil {
			log.Printf("prune logs: remove %s: %v", path, removeErr)
		} else {
			log.Printf("prune logs: removed %s", path)
		}
	}
}

func configureLogging(cfg *config.Config) (*os.File, string, error) {
	baseDir := filepath.Dir(cfg.Path())
	if baseDir == "" || baseDir == "." {
		baseDir = "."
	}

	logDir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", err
	}

	fileName := "agent-" + cfg.OfficeKey + "-" + time.Now().UTC().Format("20060102-150405") + ".log"
	logPath := filepath.Join(logDir, fileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, "", err
	}

	log.SetOutput(io.MultiWriter(logFile))
	log.SetFlags(log.Ldate | log.Ltime)
	return logFile, logPath, nil
}
