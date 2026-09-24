package setup

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"

	"null-service/internal/session"
)

//go:embed setup.html setup.css
var assets embed.FS

// ErrNotSaved is wrapped by an Activate that switched vaults successfully
// but could not persist the choice — the vault is live until restart.
var ErrNotSaved = errors.New("vault is active but the choice could not be saved")

const contentSecurityPolicy = "default-src 'none'; style-src 'self'; form-action 'self'; " +
	"base-uri 'none'; frame-ancestors 'none'"

// Handler serves /setup: pick the vault directory in the browser. Every
// route sits behind the session guard — the setup page is an admin
// surface, never open. It writes nothing itself; choosing a folder calls
// Activate, which the caller (internal/app) implements.
type Handler struct {
	Guard   *session.Guard
	Browser *Browser
	// Locked means NULL_VAULT_PATH fixes the vault: the page explains
	// that and offers no form. An operator's environment is not something
	// a browser session may override.
	Locked bool
	// Current returns the active vault path, or "" before one is chosen.
	Current func() string
	// Activate validates-and-switches to dir (already confined to the
	// browse root) and persists the choice.
	Activate func(dir string) error
	Log      *slog.Logger

	tmpl *template.Template
	csrf string
	mux  *http.ServeMux
}

// New wires a Handler. token is the UI token (the CSRF value derives
// from it, as in the renderer).
func New(h Handler, token string) (*Handler, error) {
	tmpl, err := template.ParseFS(assets, "setup.html")
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("null-csrf-v1"))
	h.tmpl, h.csrf = tmpl, hex.EncodeToString(m.Sum(nil))
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("GET /setup", h.handleGet)
	h.mux.HandleFunc("POST /setup", h.handlePost)
	h.mux.HandleFunc("GET /setup/style.css", func(w http.ResponseWriter, _ *http.Request) {
		css, _ := assets.ReadFile("setup.css")
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(css)
	})
	return &h, nil
}

// ServeHTTP handles /setup and /setup/style.css behind the guard.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Guard.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.mux.ServeHTTP(w, r)
	})).ServeHTTP(w, r)
}

type pageData struct {
	Current string
	Locked  bool
	Root    string
	L       *Listing
	CSRF    string
	Saved   bool
	Warn    string
	Err     string
	NoGit   bool
}

func (h *Handler) page(w http.ResponseWriter, status int, d pageData) {
	d.Current, d.Locked, d.Root, d.CSRF = h.Current(), h.Locked, h.Browser.Root(), h.csrf
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.tmpl.Execute(w, d); err != nil {
		h.Log.Error("setup template", "err", err)
	}
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	d := pageData{Saved: r.URL.Query().Get("saved") != ""}
	if h.Locked {
		h.page(w, http.StatusOK, d)
		return
	}
	want := r.URL.Query().Get("dir")
	if want == "" {
		want = h.Current() // start where the current vault lives, if that is browsable
	}
	dir, err := h.Browser.Resolve(want)
	if err != nil {
		if want != "" && r.URL.Query().Get("dir") != "" {
			d.Err = "That folder is outside the area this page may browse."
		}
		dir, _ = h.Browser.Resolve("")
	}
	l, err := h.Browser.List(dir)
	if err != nil {
		d.Err = "Could not read that folder."
	}
	d.L, d.NoGit = &l, !l.IsGit
	h.page(w, http.StatusOK, d)
}

func (h *Handler) handlePost(w http.ResponseWriter, r *http.Request) {
	if h.Locked {
		http.Error(w, "the vault is fixed by NULL_VAULT_PATH", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !session.SameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(h.csrf)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	dir, err := h.Browser.Resolve(r.PostForm.Get("dir"))
	if err != nil || r.PostForm.Get("dir") == "" {
		http.Error(w, "that folder cannot be used as a vault", http.StatusBadRequest)
		return
	}
	switch err := h.Activate(dir); {
	case err == nil:
		http.Redirect(w, r, "/setup?saved=1&dir="+url.QueryEscape(dir), http.StatusSeeOther)
	case errors.Is(err, ErrNotSaved):
		h.Log.Warn("vault chosen but not saved", "err", err)
		l, _ := h.Browser.List(dir)
		h.page(w, http.StatusOK, pageData{L: &l, Saved: true, Warn: "The vault is active now, but the choice could not be written to the config file, so it will be forgotten on restart: " + err.Error()})
	default:
		h.Log.Warn("vault activation failed", "dir", dir, "err", err)
		l, _ := h.Browser.List(dir)
		h.page(w, http.StatusUnprocessableEntity, pageData{L: &l, NoGit: !l.IsGit, Err: "That folder could not be opened as a vault: " + err.Error()})
	}
}
