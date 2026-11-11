// Command birdcam-server receives camera streams from birdcam agents and
// serves them to browsers via WebRTC.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"birdcam/internal/server"
	"birdcam/internal/server/config"
)

func main() {
	cfgPath := flag.String("config", "/etc/birdcam/server.yaml", "path to the YAML config file")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := run(*cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if len(cfg.Devices) == 0 {
		log.Warn("no devices configured; no agent can connect")
	}
	base := cfg.Auth.OIDC.RedirectURL
	if base == "" {
		base = cfg.ExternalURL
	}
	if cfg.Auth.Enabled() && !strings.HasPrefix(base, "https://") {
		log.Warn("auth enabled over plain http: session cookies are not marked Secure")
	}
	srv, err := server.New(server.Options{Config: cfg, Logger: log})
	if err != nil {
		return err
	}
	defer srv.Close()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("birdcam-server started", "version", server.Version, "listen", cfg.Listen,
		"ingest", fmt.Sprintf("%d-%d", cfg.Ingest.PortMin, cfg.Ingest.PortMax), "ingest_tls", cfg.Ingest.TLSEnabled(),
		"webrtc_udp", cfg.WebRTC.UDPPort, "auth", cfg.Auth.Enabled(), "external_url", cfg.ExternalURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Hijacked WebSocket connections are not tracked by Shutdown; srv.Close
	// (deferred) closes them.
	_ = httpSrv.Shutdown(shCtx)
	return nil
}
