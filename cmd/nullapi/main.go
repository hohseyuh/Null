// Command nullapi serves a markdown vault: a JSON API, a server-rendered
// HTML reader with the graph view and the Al-Mina review screen, and — on
// a fresh install — a setup page for choosing the vault in the browser.
//
// Configuration is environment-only, except the vault, which can also be
// chosen in the browser at /setup and is then remembered in a config file:
//
//	NULL_TOKEN           bearer token for the JSON API (required)
//	NULL_UI_TOKEN        token for the browser UI, Al-Mina and /setup
//	                     (default: NULL_TOKEN — set it separately so a
//	                     program holding the API token cannot approve its
//	                     own tier proposals)
//	NULL_VAULT_PATH      vault root; when set it is fixed and /setup is
//	                     read-only. When unset, the saved choice is used,
//	                     or the server starts in setup mode.
//	NULL_CONFIG_PATH     where the choice is saved (default: the user
//	                     config dir, null/config.json)
//	NULL_BROWSE_ROOT     the only area /setup may browse (default: $HOME)
//	NULL_ADDR            listen address (default ":8080")
//	NULL_MAX_BODY_BYTES  cap on note bodies returned by the API (default 200000)
//	NULL_MINA_STALE_DAYS age at which an untouched dakhil note surfaces in
//	                     Al-Mina (default 14)
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

	"null-service/internal/app"
	"null-service/internal/config"
	"null-service/internal/vault"
)

type settings struct {
	token, uiToken string
	addr           string
	maxBodyBytes   int64
	configPath     string
	browseRoot     string
}

// loadSettings reads configuration from the environment. It assumes it is
// called once at startup and fails hard on anything malformed — a
// half-configured server must not boot.
func loadSettings() (settings, error) {
	s := settings{
		token:        os.Getenv("NULL_TOKEN"),
		uiToken:      os.Getenv("NULL_UI_TOKEN"),
		addr:         os.Getenv("NULL_ADDR"),
		maxBodyBytes: 200_000,
		configPath:   config.Path(),
		browseRoot:   os.Getenv("NULL_BROWSE_ROOT"),
	}
	if s.token == "" {
		return s, errors.New("NULL_TOKEN is required")
	}
	if s.uiToken == "" {
		s.uiToken = s.token
	}
	if s.addr == "" {
		s.addr = ":8080"
	}
	if s.browseRoot == "" {
		if home, err := os.UserHomeDir(); err == nil {
			s.browseRoot = home
		} else {
			s.browseRoot = "."
		}
	}
	if v := os.Getenv("NULL_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return s, fmt.Errorf("NULL_MAX_BODY_BYTES: %q is not a positive integer", v)
		}
		s.maxBodyBytes = n
	}
	return s, nil
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
	s, err := loadSettings()
	if err != nil {
		return err
	}
	vault.GitIdentity = "nullapi" // Al-Mina commits are attributed to this binary

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	initial, envLocked, note := app.Resolve(s.configPath)
	if note != nil {
		log.Warn("starting without a vault", "reason", note)
	}
	m, err := app.New(ctx, app.Options{
		Token: s.token, UIToken: s.uiToken, MaxBodyBytes: s.maxBodyBytes,
		ConfigPath: s.configPath, BrowseRoot: s.browseRoot, EnvLocked: envLocked, Log: log,
	})
	if err != nil {
		return err
	}
	if s.uiToken == s.token {
		log.Warn("NULL_UI_TOKEN is not set: anyone holding the API token can also log in to the UI and approve tier proposals. Set a separate NULL_UI_TOKEN if a program or model uses the API.")
	}
	if initial != "" {
		if err := m.Start(initial); err != nil {
			if envLocked {
				return fmt.Errorf("NULL_VAULT_PATH: %w", err)
			}
			log.Warn("saved vault could not be opened; starting in setup mode", "vault", initial, "err", err)
		}
	}
	if m.VaultPath() == "" {
		log.Info("no vault chosen: open /setup in a browser (log in once at /login?token=<NULL_UI_TOKEN>)")
	}

	httpSrv := &http.Server{
		Addr:              s.addr,
		Handler:           m,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", s.addr, "config", s.configPath)
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
