// Command nullapi serves a read-only HTTP API over a markdown vault.
//
// Configuration is environment-only:
//
//	NULL_VAULT_PATH      path to the vault root (required)
//	NULL_TOKEN           static bearer token (required)
//	NULL_ADDR            listen address (default ":8080")
//	NULL_MAX_BODY_BYTES  cap on note bodies returned by the API (default 200000)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"null-service/internal/api"
	"null-service/internal/render"
	"null-service/internal/search"
	"null-service/internal/vault"
)

type config struct {
	vaultPath    string
	token        string
	addr         string
	maxBodyBytes int64
}

// loadConfig reads configuration from the environment. It assumes it is
// called once at startup and fails hard on anything missing or malformed —
// a half-configured server must not boot.
func loadConfig() (config, error) {
	cfg := config{
		vaultPath:    os.Getenv("NULL_VAULT_PATH"),
		token:        os.Getenv("NULL_TOKEN"),
		addr:         os.Getenv("NULL_ADDR"),
		maxBodyBytes: 200_000,
	}
	if cfg.vaultPath == "" {
		return cfg, errors.New("NULL_VAULT_PATH is required")
	}
	info, err := os.Stat(cfg.vaultPath)
	if err != nil {
		return cfg, fmt.Errorf("NULL_VAULT_PATH: %w", err)
	}
	if !info.IsDir() {
		return cfg, fmt.Errorf("NULL_VAULT_PATH: %s is not a directory", cfg.vaultPath)
	}
	if cfg.token == "" {
		return cfg, errors.New("NULL_TOKEN is required")
	}
	if cfg.addr == "" {
		cfg.addr = ":8080"
	}
	if v := os.Getenv("NULL_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("NULL_MAX_BODY_BYTES: %q is not a positive integer", v)
		}
		cfg.maxBodyBytes = n
	}
	return cfg, nil
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// run boots the server and blocks until SIGINT/SIGTERM, then shuts down
// gracefully. It assumes it owns the process lifecycle: it is the only
// place in the program allowed to exit.
func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	searcher, err := search.New(cfg.vaultPath)
	if err != nil {
		return err // missing rg is a startup error, never a runtime 500
	}

	ix := vault.NewIndex(cfg.vaultPath, log)
	if err := ix.Build(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	watcher, err := vault.NewWatcher(ix, 200*time.Millisecond)
	if err != nil {
		return err
	}
	go watcher.Run(ctx)

	renderer, err := render.New(ix, searcher, cfg.vaultPath, cfg.token, log)
	if err != nil {
		return err
	}

	srv := &api.Server{
		Token:        cfg.token,
		MaxBodyBytes: cfg.maxBodyBytes,
		VaultRoot:    cfg.vaultPath,
		Index:        ix,
		Search:       searcher,
		Renderer:     renderer,
		Log:          log,
	}

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.addr, "vault", cfg.vaultPath)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
