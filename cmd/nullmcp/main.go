// Command nullmcp exposes the vault as Model Context Protocol tools —
// list_notes, get_note, search_notes, get_graph, find_relatives,
// get_links, find_path, and (when NULL_INBOX_PATH is set) create_note
// and write_note — over stdio, for a local LLM client (Claude Desktop,
// Claude Code, a future Basim process) to launch as a subprocess.
//
// Same vault, same vault/search packages, same path-safety and body-cap
// rules as nullapi: this is a second presentation of the read API, not a
// second implementation of it.
//
// Writes: the vault at NULL_VAULT_PATH is never opened for anything but
// reading, full stop — that non-negotiable is unchanged. The one
// sanctioned exception is a second, physically separate directory,
// NULL_INBOX_PATH: create_note/write_note touch only that root. Inbox
// notes are merged into every read tool's results, addressed as
// "inbox/<path>" and labeled, so a model sees its own drafts — but
// promoting a draft into the real vault stays a human, git-mediated act
// this server never performs. See spec/null-mcp-v0.md.
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
	"path/filepath"
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
	inboxPath     string // empty disables create_note/write_note and inbox visibility
	maxBodyBytes  int64
	inspectorAddr string // empty disables the inspector
	httpAddr      string // empty means stdio transport; set means HTTP-only, see package doc
	token         string // required iff httpAddr is set
}

// loadConfig reads configuration from the environment. It assumes it is
// called once at startup and fails hard on anything missing or
// malformed.
func loadConfig() (config, error) {
	cfg := config{
		vaultPath:     os.Getenv("NULL_VAULT_PATH"),
		inboxPath:     os.Getenv("NULL_INBOX_PATH"),
		maxBodyBytes:  200_000,
		inspectorAddr: os.Getenv("NULL_MCP_INSPECTOR_ADDR"),
		httpAddr:      os.Getenv("NULL_MCP_HTTP_ADDR"),
		token:         os.Getenv("NULL_TOKEN"),
	}
	if cfg.vaultPath == "" {
		return cfg, errors.New("NULL_VAULT_PATH is required")
	}
	if err := requireDir("NULL_VAULT_PATH", cfg.vaultPath); err != nil {
		return cfg, err
	}
	if cfg.inboxPath != "" {
		if err := requireDir("NULL_INBOX_PATH", cfg.inboxPath); err != nil {
			return cfg, err
		}
		// A vault-side top-level "inbox/" would collide with the
		// reserved merged namespace (see internal/mcp.InboxPrefix) —
		// fail loudly at boot rather than silently shadowing real notes.
		if info, err := os.Stat(filepath.Join(cfg.vaultPath, "inbox")); err == nil && info.IsDir() {
			return cfg, fmt.Errorf(
				"NULL_VAULT_PATH has a top-level 'inbox/' directory, which collides with the reserved inbox namespace; rename it")
		}
	}
	if v := os.Getenv("NULL_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("NULL_MAX_BODY_BYTES: %q is not a positive integer", v)
		}
		cfg.maxBodyBytes = n
	}
	if cfg.httpAddr != "" && cfg.token == "" {
		return cfg, errors.New("NULL_TOKEN is required when NULL_MCP_HTTP_ADDR is set")
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

	vaultIndex := vault.NewIndex(cfg.vaultPath, log)
	if err := vaultIndex.Build(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	vaultWatcher, err := vault.NewWatcher(vaultIndex, 200*time.Millisecond)
	if err != nil {
		return err
	}
	go vaultWatcher.Run(ctx)

	// Reader defaults to the plain vault index; wiring in the inbox
	// below, when configured, replaces it with a vault.Combined that
	// merges the two — every read tool sees whichever Reader ends up
	// here, unchanged code either way.
	var index vault.Reader = vaultIndex
	var inboxSearcher *search.Searcher
	var inboxIndex *vault.Index

	if cfg.inboxPath != "" {
		inboxSearcher, err = search.New(cfg.inboxPath)
		if err != nil {
			return fmt.Errorf("inbox search: %w", err)
		}

		inboxIndex = vault.NewIndex(cfg.inboxPath, log)
		inboxIndex.Source = vault.SourceInbox
		if err := inboxIndex.Build(); err != nil {
			return err
		}
		inboxWatcher, err := vault.NewWatcher(inboxIndex, 200*time.Millisecond)
		if err != nil {
			return err
		}
		go inboxWatcher.Run(ctx)

		index = vault.NewCombined(vaultIndex, inboxIndex, nullmcp.InboxPrefix)
		log.Info("inbox configured", "path", cfg.inboxPath, "notes", inboxIndex.Len())
	}

	server := nullmcp.NewServer(&nullmcp.Tools{
		Index:        index,
		VaultIndex:   vaultIndex,
		Search:       searcher,
		InboxSearch:  inboxSearcher,
		VaultRoot:    cfg.vaultPath,
		InboxRoot:    cfg.inboxPath,
		InboxIndex:   inboxIndex,
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
		return runHTTPTransport(ctx, server, cfg.httpAddr, cfg.token, log)
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
func runHTTPTransport(ctx context.Context, server *sdkmcp.Server, addr, token string, log *slog.Logger) error {
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           nullmcp.NewHTTPHandler(server, token, log),
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
