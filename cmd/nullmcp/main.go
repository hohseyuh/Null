// Command nullmcp exposes the vault as Model Context Protocol tools —
// list_notes, get_note, search_notes, get_graph — over stdio, for a
// local LLM client (Claude Desktop, Claude Code, a future Basim process)
// to launch as a subprocess.
//
// Same vault, same vault/search packages, same path-safety and body-cap
// rules as nullapi: this is a second presentation of the read API, not a
// second implementation of it.
//
// Auth: stdio's trust boundary is the OS process — whoever can spawn this
// binary already has the access a bearer token would gate over HTTP, so
// none is required here. If an HTTP/SSE transport is added later (the
// SDK supports it; see internal/mcp/server.go's NewServer, which is
// transport-agnostic), it must gate on NULL_TOKEN with the same
// constant-time compare internal/api/auth.go uses before it is exposed
// off-loopback — that is not yet wired up.
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

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	nullmcp "null-service/internal/mcp"
	"null-service/internal/search"
	"null-service/internal/vault"
)

type config struct {
	vaultPath     string
	maxBodyBytes  int64
	inspectorAddr string // empty disables the inspector
}

// loadConfig reads configuration from the environment. It assumes it is
// called once at startup and fails hard on anything missing or
// malformed.
func loadConfig() (config, error) {
	cfg := config{
		vaultPath:     os.Getenv("NULL_VAULT_PATH"),
		maxBodyBytes:  200_000,
		inspectorAddr: os.Getenv("NULL_MCP_INSPECTOR_ADDR"),
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

	server := nullmcp.NewServer(ix, searcher, cfg.vaultPath, cfg.maxBodyBytes, log)

	if cfg.inspectorAddr != "" {
		if err := startInspector(ctx, server, cfg.inspectorAddr, log); err != nil {
			return fmt.Errorf("inspector: %w", err)
		}
	}

	log.Info("mcp server starting", "vault", cfg.vaultPath, "notes", ix.Len())
	if err := server.Run(ctx, &sdkmcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
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
