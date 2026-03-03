package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/antoinecorbel7/plex-tunnel/pkg/relay"
)

func main() {
	cfg, err := relay.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load relay config: %v\n", err)
		os.Exit(1)
	}

	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid relay log level %q: %v\n", cfg.LogLevel, err)
		os.Exit(1)
	}
	zerolog.SetGlobalLevel(level)

	logger := zerolog.New(os.Stdout).With().Timestamp().Str("component", "relay").Logger()

	runner, err := relay.New(cfg, logger)
	if err != nil {
		logger.Error().Err(err).Msg("failed to initialize relay")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := runner.Run(ctx); err != nil {
		logger.Error().Err(err).Msg("relay stopped with error")
		os.Exit(1)
	}

	logger.Info().Msg("relay shutdown complete")
}
