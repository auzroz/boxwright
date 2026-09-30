// Command server runs the Boxwright backend.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boxwright/internal/ai"
	"boxwright/internal/api"
	"boxwright/internal/config"
	"boxwright/internal/homebox"
)

// version is stamped at build time with -ldflags "-X main.version=...". The
// release workflow passes the git tag; anything else reports "dev".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.HomeboxAPIKey == "" && !cfg.AllowClientHomebox {
		log.Warn("HOMEBOX_API_KEY is not set; Homebox calls will fail until configured")
	}
	if cfg.AllowClientHomebox {
		log.Warn("ALLOW_CLIENT_HOMEBOX=true: a request may name its own Homebox URL and token, " +
			"so an authenticated caller can make this server connect to any address it can reach. " +
			"Intended for a backend serving people who each have their own Homebox; leave it off " +
			"for a single-user deployment.")
	}

	identifier, err := ai.New(cfg.AIProvider, cfg.AIBaseURL, cfg.AIAPIKey, cfg.AIModel,
		ai.WithEffort(cfg.AIEffort), ai.WithThinking(cfg.AIThinking))
	if err != nil {
		return err
	}

	hb := homebox.New(cfg.HomeboxBaseURL, cfg.HomeboxAPIKey)
	srv := api.New(hb, identifier, cfg.BoxCacheTTL, log)
	srv.RequireToken(cfg.APIToken)
	srv.AllowClientCredentials(cfg.AllowClientHomebox)
	srv.SetAbout(api.About{Version: version, AIProvider: cfg.AIProvider})
	if cfg.HomeboxWatch {
		srv.StartWatching()
		// Covers the error returns below; the ordered Close after Shutdown is
		// the one that carries the meaning.
		defer srv.Close()
	}

	httpSrv := &http.Server{
		Addr:    net.JoinHostPort(cfg.ListenAddr, cfg.Port),
		Handler: srv.Routes(),
		// Without these a stalled client holds a connection forever. The write
		// timeout is generous because /identify waits on a vision model, which
		// can legitimately take a minute on local hardware.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      6 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	// Signal handling before ListenAndServe, so a SIGTERM arriving during
	// startup is not lost.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("boxwright backend listening", "version", version, "addr", httpSrv.Addr, "config", cfg)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Let in-flight work finish. A catalog request killed mid-write leaves an
	// entity in Homebox with no record on our side of whether it landed.
	log.Info("shutting down; waiting for in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	// After Shutdown returns, not before: a catalog request draining through
	// that window is the one most in need of a cache that still learns about
	// edits. The watcher holds its own context rather than this one, which is
	// what lets the stop happen here instead of the moment SIGTERM arrived.
	if err := srv.Close(); err != nil {
		return err
	}
	log.Info("stopped cleanly")
	return nil
}
