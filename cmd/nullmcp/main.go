// Command nullmcp exposes the vault as Model Context Protocol tools —
// list_notes, get_note, search_notes, get_graph, find_relatives,
// get_links, find_path, create_note, write_note, delete_note, tier_get,
// tier_set, and tier_propose — over stdio, for a local LLM client
// (Claude Desktop, Claude Code, a future Basim process) to launch as a
// subprocess.
//
// Same vault, same vault/search packages, same path-safety rules as
// nullapi: this is a second presentation of the read API, and now also
// the write path — see CLAUDE.md's non-negotiable #2 and
// spec/null-mcp-v0.md. Writes go straight to NULL_VAULT_PATH; the vault
// must be a git repository (checked at boot, see vault.EnsureGitRepo),
// because every create_note, write_note, and delete_note becomes its own
// isolated commit. There is no push or commit tool exposed to the model
// anywhere in this binary — see spec/tiers.md's "One door" — the server
// commits on write, and a human pushes with their own `git push`.
//
// Every note carries a tier (spec/tiers.md): dakhil, amil, thabit, or
// asil, server-owned. tier_set can only lower a tier (R1); only a human,
// via Al-Mina, ever raises one.
//
// Transport: stdio by default (auth is implicit — the OS process
// boundary is whoever can spawn this binary). Setting NULL_MCP_HTTP_ADDR
// switches to the Streamable HTTP transport instead — never both at
// once: this process's stdin has no real client on it when run as a
// daemon behind a reverse proxy, and reading from it would just EOF the
// moment nothing writes to it, exiting the process (this is not
// hypothetical — it happened during development the first time this
// binary was smoke-tested backgrounded without a client attached).
// NULL_TOKEN is required whenever NULL_MCP_HTTP_ADDR is set, checked
// with the same constant-time compare internal/api/auth.go uses; see
// internal/mcp/http.go.
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
	"strings"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	appconfig "null-service/internal/config"
	nullmcp "null-service/internal/mcp"
	"null-service/internal/search"
	"null-service/internal/vault"
)

type config struct {
	vaultPath     string
	maxBodyBytes  int64
	inspectorAddr string // empty disables the inspector
	httpAddr      string // empty means stdio transport; set means HTTP-only, see package doc
	token         string // required iff httpAddr is set
	publicURL     string // required iff httpAddr is set; this server's own public https origin
}

// loadConfig reads configuration from the environment. It assumes it is
// called once at startup and fails hard on anything missing or
// malformed.
func loadConfig() (config, error) {
	// The vault comes from NULL_VAULT_PATH, or failing that from the file
	// nullapi's /setup page saves — so a vault chosen in the browser is the
	// one the model writes to, too (read once, at boot).
	vaultPath, _, err := appconfig.VaultPath(appconfig.Path())
	if err != nil {
		return config{}, err
	}
	cfg := config{
		vaultPath:     vaultPath,
		maxBodyBytes:  200_000,
		inspectorAddr: os.Getenv("NULL_MCP_INSPECTOR_ADDR"),
		httpAddr:      os.Getenv("NULL_MCP_HTTP_ADDR"),
		token:         os.Getenv("NULL_TOKEN"),
		publicURL:     strings.TrimSuffix(os.Getenv("NULL_MCP_PUBLIC_URL"), "/"),
	}
	if cfg.vaultPath == "" {
		return cfg, errors.New("no vault configured: set NULL_VAULT_PATH, or choose one in nullapi's /setup page (this reads the same saved config)")
	}
	if err := requireDir("vault", cfg.vaultPath); err != nil {
		return cfg, err
	}
	// Every write commits, so the vault must already be a git repository —
	// fail loudly at boot, never on the first write attempt at runtime.
	if err := vault.EnsureGitRepo(cfg.vaultPath); err != nil {
		return cfg, err
	}
	if v := os.Getenv("NULL_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("NULL_MAX_BODY_BYTES: %q is not a positive integer", v)
		}
		cfg.maxBodyBytes = n
	}
	if cfg.httpAddr != "" {
		if cfg.token == "" {
			return cfg, errors.New("NULL_TOKEN is required when NULL_MCP_HTTP_ADDR is set")
		}
		if cfg.publicURL == "" {
			return cfg, errors.New("NULL_MCP_PUBLIC_URL is required when NULL_MCP_HTTP_ADDR is set " +
				"(the public https origin a reverse proxy exposes this on, e.g. https://host:10000 — " +
				"needed for OAuth discovery metadata; see spec/null-mcp-v0.md)")
		}
		if !strings.HasPrefix(cfg.publicURL, "https://") && !strings.HasPrefix(cfg.publicURL, "http://localhost") {
			return cfg, fmt.Errorf("NULL_MCP_PUBLIC_URL: %q must be https:// (or http://localhost for local dev)", cfg.publicURL)
		}
	}
	return cfg, nil
}

func requireDir(envVar, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", envVar, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: %s is not a directory", envVar, path)
	}
	return nil
}

func main() {
	// stdout carries the MCP protocol; logs must never land there.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	searcher, err := search.New(cfg.vaultPath)
	if err != nil {
		return err // missing rg is a startup error, never a runtime failure
	}

	index := vault.NewIndex(cfg.vaultPath, log)
	if err := index.Build(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	watcher, err := vault.NewWatcher(index, 200*time.Millisecond)
	if err != nil {
		return err
	}
	go watcher.Run(ctx)

	server := nullmcp.NewServer(&nullmcp.Tools{
		Index:        index,
		Search:       searcher,
		VaultRoot:    cfg.vaultPath,
		MaxBodyBytes: cfg.maxBodyBytes,
		Log:          log,
	})

	if cfg.inspectorAddr != "" {
		if err := startInspector(ctx, server, cfg.inspectorAddr, log); err != nil {
			return fmt.Errorf("inspector: %w", err)
		}
	}

	log.Info("mcp server starting", "vault", cfg.vaultPath, "notes", index.Len())

	if cfg.httpAddr != "" {
		oauth, err := nullmcp.NewOAuthServer(cfg.publicURL, cfg.token)
		if err != nil {
			return err
		}
		go sweepOAuthPeriodically(ctx, oauth)
		return runHTTPTransport(ctx, server, oauth, cfg.httpAddr, cfg.token, log)
	}
	if err := server.Run(ctx, &sdkmcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}

// runHTTPTransport serves the MCP Streamable HTTP transport at addr
// until ctx is cancelled, then shuts down gracefully — the HTTP-mode
// equivalent of server.Run(ctx, &sdkmcp.StdioTransport{}), blocking the
// same way so run's control flow doesn't need to know which transport
// is active. addr should be a loopback address; a reverse proxy (see
// the package doc) is what makes it reachable from anywhere else, and
// is also what terminates TLS — this process never does.
func runHTTPTransport(ctx context.Context, server *sdkmcp.Server, oauth *nullmcp.OAuthServer, addr, token string, log *slog.Logger) error {
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           nullmcp.NewHTTPHandler(server, oauth, token, log),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("mcp http listening", "addr", addr, "path", nullmcp.HTTPPath)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("mcp http: %w", err)
	case <-ctx.Done():
	}

	log.Info("mcp http shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("mcp http shutdown: %w", err)
	}
	return nil
}

// sweepOAuthPeriodically evicts expired codes/tokens from oauth every
// few minutes until ctx is cancelled. Purely memory hygiene on a
// long-running process — every lookup path also checks expiry itself,
// so correctness never depends on this running.
func sweepOAuthPeriodically(ctx context.Context, oauth *nullmcp.OAuthServer) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			oauth.Sweep()
		}
	}
}

// startInspector wires the dev-only tool inspector to its own HTTP
// listener, shut down alongside the main context. It assumes addr is a
// deliberate choice by whoever set NULL_MCP_INSPECTOR_ADDR — this
// function does not default or validate beyond what net/http already
// does, and does not add auth (see the package doc comment).
func startInspector(ctx context.Context, server *sdkmcp.Server, addr string, log *slog.Logger) error {
	ins, err := nullmcp.NewInspector(ctx, server, log)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Addr: addr, Handler: ins, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info("inspector listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("inspector failed", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()
	return nil
}
