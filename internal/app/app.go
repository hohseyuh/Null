// Package app owns the running nullapi: it builds the index, watcher,
// search, JSON API and renderer for one vault, and can replace them with
// a different vault at runtime — which is what lets the first-run setup
// page choose a folder without restarting the process. Until a vault is
// chosen it serves only the setup page.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"null-service/internal/api"
	"null-service/internal/config"
	"null-service/internal/render"
	"null-service/internal/search"
	"null-service/internal/session"
	"null-service/internal/setup"
	"null-service/internal/vault"
)

// Options configures a Manager.
type Options struct {
	// Token gates the JSON API. UIToken gates the renderer, Al-Mina and
	// the setup page; empty means "same as Token". Keeping them distinct
	// is what stops a program holding the API token from approving its own
	// tier proposals.
	Token, UIToken string
	MaxBodyBytes   int64
	ConfigPath     string
	BrowseRoot     string
	// EnvLocked means NULL_VAULT_PATH fixed the vault; setup is read-only.
	EnvLocked bool
	Log       *slog.Logger
}

type running struct {
	handler http.Handler
	cancel  context.CancelFunc
	path    string
}

// Manager is the top-level http.Handler.
type Manager struct {
	opts  Options
	cur   atomic.Pointer[running]
	setup *setup.Handler
	guard *session.Guard

	mu     sync.Mutex // serializes Start
	rootCx context.Context
}

// New returns a Manager with no vault active. Call Start to activate one.
// ctx bounds every watcher the Manager ever starts.
func New(ctx context.Context, opts Options) (*Manager, error) {
	if opts.UIToken == "" {
		opts.UIToken = opts.Token
	}
	// The folder browser only matters when the vault is chosen in the
	// browser. With NULL_VAULT_PATH fixing it, a missing browse root (the
	// image defaults it to /vaults, which a single-vault compose file never
	// mounts) must not stop the server from starting.
	b, err := setup.NewBrowser(opts.BrowseRoot)
	if err != nil && !opts.EnvLocked {
		return nil, fmt.Errorf("%w (choosing a vault in the browser needs a folder to browse: mount one there, point NULL_BROWSE_ROOT at one, or set NULL_VAULT_PATH to fix the vault instead)", err)
	}
	m := &Manager{opts: opts, rootCx: ctx, guard: session.NewGuard(opts.UIToken)}
	m.setup, err = setup.New(setup.Handler{
		Guard: m.guard, Browser: b, Locked: opts.EnvLocked,
		Current: m.VaultPath, Activate: m.activateFromSetup, Log: opts.Log,
	}, opts.UIToken)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// VaultPath returns the active vault, or "" in setup mode.
func (m *Manager) VaultPath() string {
	if r := m.cur.Load(); r != nil {
		return r.path
	}
	return ""
}

// Start builds everything over path and swaps it in, stopping the
// previous vault's watcher. On any error the previous vault (if any)
// keeps serving untouched. It assumes path already passed whatever
// confinement the caller requires — Start itself only checks it is a
// directory.
func (m *Manager) Start(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	log := m.opts.Log
	searcher, err := search.New(path)
	if err != nil {
		return err // missing rg is a startup error, never a runtime 500
	}
	ix := vault.NewIndex(path, log)
	if err := ix.Build(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(m.rootCx)
	watcher, err := vault.NewWatcher(ix, 200*time.Millisecond)
	if err != nil {
		cancel()
		return err
	}
	go watcher.Run(ctx)

	rd, err := render.New(ix, searcher, path, m.opts.UIToken, log)
	if err != nil {
		cancel()
		return err
	}
	if err := vault.EnsureGitRepo(path); err != nil {
		log.Warn("vault is not a git repository: reading works, but Al-Mina decisions and first-open promotion are disabled", "vault", path, "err", err)
	} else {
		rd.Writable = true
	}
	srv := &api.Server{
		Token: m.opts.Token, MaxBodyBytes: m.opts.MaxBodyBytes, VaultRoot: path,
		Index: ix, Search: searcher, Renderer: rd, Log: log,
	}
	next := &running{handler: srv.Router(), cancel: cancel, path: path}
	if old := m.cur.Swap(next); old != nil {
		old.cancel()
	}
	log.Info("vault active", "vault", path, "notes", ix.Len(), "writable", rd.Writable)
	return nil
}

// activateFromSetup is the setup page's Activate: switch, then persist.
func (m *Manager) activateFromSetup(dir string) error {
	if err := m.Start(dir); err != nil {
		return err
	}
	if err := config.Save(m.opts.ConfigPath, config.File{VaultPath: dir}); err != nil {
		return fmt.Errorf("%w: %v", setup.ErrNotSaved, err)
	}
	return nil
}

// ServeHTTP routes /setup to the setup page, everything else to the
// active vault, or — before one is chosen — to a redirect toward setup
// (and a 503 JSON body for API clients, who cannot follow one).
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/setup" || strings.HasPrefix(r.URL.Path, "/setup/") {
		m.setup.ServeHTTP(w, r)
		return
	}
	if run := m.cur.Load(); run != nil {
		run.handler.ServeHTTP(w, r)
		return
	}
	switch {
	case r.URL.Path == "/login":
		m.guard.Login(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/mina":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"not_configured","detail":"no vault chosen yet; open /setup in a browser"}`))
	default:
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	}
}

// Resolve picks the initial vault: NULL_VAULT_PATH, else the saved
// config, else none (setup mode). A saved path that has since vanished
// yields none plus a reason to log, not a crash — the setup page is how
// you fix that.
func Resolve(configPath string) (path string, envLocked bool, note error) {
	p, src, err := config.VaultPath(configPath)
	if err != nil {
		return "", false, err
	}
	switch src {
	case "env":
		return p, true, nil
	case "file":
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			return "", false, errors.New("saved vault " + filepath.Clean(p) + " is no longer a directory; choose it again in /setup")
		}
	}
	return p, false, nil
}
