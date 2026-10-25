package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"databasus-verification-container-agent/internal/config"
	"databasus-verification-container-agent/internal/features/start"
	agentlogger "databasus-verification-container-agent/internal/logger"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.LoadFromEnvironment()
	if err != nil {
		logger.Error("invalid agent configuration", "error", err)
		os.Exit(1)
	}
	logger = agentlogger.NewLogger(os.Stdout, cfg.Token)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	if err := start.Run(ctx, cfg, logger); err != nil {
		logger.Error("verification agent failed", "error", err)
		cancel()
		os.Exit(1)
	}
	cancel()
}
