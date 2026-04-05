package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/CRBL-Technologies/plex-tunnel/pkg/client"
)

func main() {
	cfg, err := client.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load client config: %v\n", err)
		os.Exit(1)
	}

	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid client log level %q: %v\n", cfg.LogLevel, err)
		os.Exit(1)
	}
	zerolog.SetGlobalLevel(level)

	logger := zerolog.New(os.Stdout).With().Timestamp().Str("component", "client").Logger()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	controller := newClientController(ctx, cfg, logger)
	controller.Start()
	defer controller.Stop()

	uiListen := getenvDefault("PLEXTUNNEL_UI_LISTEN", "127.0.0.1:9090")
	uiPassword := os.Getenv("PLEXTUNNEL_UI_PASSWORD")
	if uiListen != "" {
		if !isLoopbackUIListen(uiListen) && uiPassword == "" {
			logger.Fatal().Msg("UI bound to non-loopback address without password — set PLEXTUNNEL_UI_PASSWORD to protect it")
		}

		srv := &http.Server{
			Addr:              uiListen,
			Handler:           newUIHandler(controller, logger, uiPassword, uiListen),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}

		go func() {
			logger.Info().Str("addr", uiListen).Msg("client web UI listening")
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error().Err(err).Msg("client web UI stopped with error")
				cancel()
			}
		}()

		<-ctx.Done()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn().Err(err).Msg("client web UI shutdown error")
		}
	} else {
		<-ctx.Done()
	}

	logger.Info().Msg("client shutdown complete")
}

func getenvDefault(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func isLoopbackUIListen(addr string) bool {
	host := strings.TrimSpace(addr)
	if host == "" || strings.HasPrefix(host, ":") {
		return false
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
