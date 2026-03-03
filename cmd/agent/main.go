package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/antoinecorbel7/plex-tunnel/pkg/agent"
)

func main() {
	cfg, err := agent.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load agent config: %v\n", err)
		os.Exit(1)
	}

	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid agent log level %q: %v\n", cfg.LogLevel, err)
		os.Exit(1)
	}
	zerolog.SetGlobalLevel(level)

	logger := zerolog.New(os.Stdout).With().Timestamp().Str("component", "agent").Logger()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runner := agent.New(cfg, logger)
	if err := runner.Run(ctx); err != nil {
		logger.Error().Err(err).Msg("agent stopped with error")
		os.Exit(1)
	}

	logger.Info().Msg("agent shutdown complete")
}
