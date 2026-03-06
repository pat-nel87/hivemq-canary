package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hivemq-canary/internal/alerts"
	"hivemq-canary/internal/blobstore"
	"hivemq-canary/internal/canary"
	"hivemq-canary/internal/config"
	"hivemq-canary/internal/metrics"
	"hivemq-canary/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	// Structured JSON logging
	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	logger.Info("config loaded",
		"broker", cfg.Broker.Host,
		"canary_interval", cfg.Canary.Interval,
		"alert_rules", len(cfg.Alerts.Rules),
	)

	// Shared in-memory ring buffer: 2880 samples = 24h at 30s interval
	buffer := metrics.NewRingBuffer(2880)

	// Create components
	alertEngine := alerts.NewEngine(cfg.Alerts, cfg.Broker.Host, buffer, logger)
	blobStore := blobstore.New(cfg.Storage.AzureBlob, logger)
	canaryClient := canary.New(cfg.Broker, cfg.Canary, buffer, logger)
	canaryClient.AddSink(blobStore)
	srv := server.New(cfg.Server, cfg.Broker.Host, buffer, alertEngine, logger)

	// Graceful shutdown context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 4)

	go func() { errCh <- canaryClient.Start(ctx) }()
	go func() { alertEngine.Start(ctx) }()
	go func() { blobStore.StartFlusher(ctx, 1*time.Minute) }()
	go func() { errCh <- srv.Start(ctx) }()

	logger.Info("hivemq-canary started",
		"dashboard_port", cfg.Server.Port,
		"broker", cfg.Broker.Host,
	)

	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	case err := <-errCh:
		if err != nil {
			logger.Error("component error", "error", err)
			cancel()
		}
	}

	time.Sleep(2 * time.Second)
	logger.Info("hivemq-canary stopped")
}
