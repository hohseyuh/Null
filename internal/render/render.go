// Package render is the read-only HTML face of the vault. It reads the
// same in-memory index as the JSON API — one process, two presentations —
// and never makes an HTTP call to its own service.
package render

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"null-service/internal/search"
	"null-service/internal/vault"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/style.css
var styleCSS []byte

// cookieName holds the bearer token for browsers; set by GET /login.
const cookieName = "null_token"

// Renderer serves the HTML views. All fields must be set before Mount.
type Renderer struct {
	Index     *vault.Index
	Search    *search.Searcher
	VaultRoot string
	Token     string
	Log       *slog.Logger

	tmpl      *template.Template
	tokenHash [32]byte
}

// New parses the embedded templates and returns a Renderer ready to
// Mount. It assumes root is the same vault root the index was built over.
func New(ix *vault.Index, s *search.Searcher, root, token string, log *slog.Logger) (*Renderer, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"date": func(t time.Time) string { return t.Format("2006-01-02") },
		"noteURL": func(p string) string {
			return (&url.URL{Path: "/n/" + p}).EscapedPath()
		},
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Renderer{
		Index:     ix,
		Search:    s,
		VaultRoot: root,
		Token:     token,
		Log:       log,
		tmpl:      tmpl,
		tokenHash: sha256.Sum256([]byte(token)),
	}, nil
}

// Mount registers the HTML routes on r. /login is the only unauthenticated
// route; everything else requires the bearer token, via header or cookie.
func (rd *Renderer) Mount(r chi.Router) {
	r.Get("/login", rd.handleLogin)
	r.Group(func(r chi.Router) {
		r.Use(rd.requireToken)
		r.Get("/", rd.handleList)
		r.Get("/n/*", rd.handleNote)
		r.Get("/s", rd.handleSearch)
		r.Get("/static/style.css", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Write(styleCSS)
		})
	})
}

// requireToken accepts the API bearer header or the login cookie, both
// compared constant-time against the configured token.
func (rd *Renderer) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidate := ""
		if h, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			candidate = h
		} else if c, err := r.Cookie(cookieName); err == nil {
			candidate = c.Value
		}
		got := sha256.Sum256([]byte(candidate))
		if subtle.ConstantTimeCompare(got[:], rd.tokenHash[:]) != 1 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>401</title>`+
				`<body style="background:#14161a;color:#c9cdd3;font:16px/1.6 system-ui;padding:4rem">`+
				`<p>Unauthorized. Visit <code>/login?token=&lt;token&gt;</code> once; a cookie will keep you in.</p>`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleLogin sets the token cookie so a browser can hold the bearer
// token. Deliberately not a form — the token travels in the query string
// exactly once, over TLS in deployment.
func (rd *Renderer) handleLogin(w http.ResponseWriter, r *http.Request) {
	got := sha256.Sum256([]byte(r.URL.Query().Get("token")))
	if subtle.ConstantTimeCompare(got[:], rd.tokenHash[:]) != 1 {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    r.URL.Query().Get("token"),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (rd *Renderer) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := rd.tmpl.ExecuteTemplate(w, name, data); err != nil {
		rd.Log.Error("template", "name", name, "err", err)
	}
}

// folderGroup is one directory's notes on the list page.
type folderGroup struct {
	Folder string
	Notes  []*vault.Note
}

// handleList serves GET /: every note, grouped by folder, newest first
// within each group.
func (rd *Renderer) handleList(w http.ResponseWriter, _ *http.Request) {
	byFolder := map[string][]*vault.Note{}
	for _, n := range rd.Index.All() {
		dir := path.Dir(n.Path)
		if dir == "." {
			dir = ""
		}
		byFolder[dir] = append(byFolder[dir], n)
	}
	groups := make([]folderGroup, 0, len(byFolder))
	for dir, notes := range byFolder {
		sort.Slice(notes, func(i, j int) bool {
			if !notes[i].UpdatedAt.Equal(notes[j].UpdatedAt) {
				return notes[i].UpdatedAt.After(notes[j].UpdatedAt)
			}
			return notes[i].Path < notes[j].Path
		})
		groups = append(groups, folderGroup{Folder: dir, Notes: notes})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Folder < groups[j].Folder })

	rd.render(w, http.StatusOK, "list.html", map[string]any{
		"Title":  "Null",
		"Groups": groups,
		"Count":  rd.Index.Len(),
	})
}

// handleNote serves GET /n/{path...}: the note as HTML, frontmatter as a
// header block, backlinks with their context lines at the bottom.
func (rd *Renderer) handleNote(w http.ResponseWriter, r *http.Request) {
	rel, err := vault.SafeRequestPath(rd.VaultRoot, strings.TrimPrefix(r.URL.Path, "/n/"))
	if err != nil {
		// a browser gets one answer for unsafe, hidden, and absent alike
		rd.notFound(w)
		return
	}
	n, ok := rd.Index.Get(rel)
	if !ok {
		rd.notFound(w)
		return
	}

	rd.render(w, http.StatusOK, "note.html", map[string]any{
		"Title":       n.Title,
		"Note":        n,
		"Frontmatter": frontmatterRows(n.Frontmatter),
		"Body":        rd.markdown(n.Body),
		"Backlinks":   rd.Index.Backlinks(n.Path),
	})
}

func (rd *Renderer) notFound(w http.ResponseWriter) {
	rd.render(w, http.StatusNotFound, "notfound.html", map[string]any{"Title": "not found"})
}

// handleSearch serves GET /s?q=: search results with highlighted snippets.
func (rd *Renderer) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	type hit struct {
		Path     string
		Title    string
		Snippets []template.HTML
	}
	var hits []hit
	if q != "" {
		results, err := rd.Search.Search(r.Context(), q)
		if err != nil {
			rd.Log.Error("render search", "q", q, "err", err)
			http.Error(w, "search failed", http.StatusInternalServerError)
			return
		}
		for _, res := range results {
			n, ok := rd.Index.Get(res.Path)
			if !ok {
				continue
			}
			h := hit{Path: res.Path, Title: n.Title}
			for _, m := range res.Matches {
				h.Snippets = append(h.Snippets, highlight(m.Snippet, q))
			}
			hits = append(hits, h)
		}
	}
	rd.render(w, http.StatusOK, "search.html", map[string]any{
		"Title": "search", "Query": q, "Hits": hits,
	})
}

// frontmatterRow is one key/value pair of the note header block.
type frontmatterRow struct {
	Key   string
	Value string
}

// frontmatterRows flattens frontmatter into sorted display rows — a small
// header block, not raw YAML.
func frontmatterRows(fm map[string]any) []frontmatterRow {
	rows := make([]frontmatterRow, 0, len(fm))
	for k, v := range fm {
		rows = append(rows, frontmatterRow{Key: k, Value: flatten(v)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows
}

func flatten(v any) string {
	switch t := v.(type) {
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = flatten(e)
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(v)
	}
}
