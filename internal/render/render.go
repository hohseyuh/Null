// Package render is the read-only HTML face of the vault. It reads the
// same in-memory index as the JSON API — one process, two presentations —
// and never makes an HTTP call to its own service.
package render

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"null-service/internal/search"
	"null-service/internal/session"
	"null-service/internal/vault"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// cookieName holds the bearer token for browsers; set by GET /login.
const cookieName = session.CookieName

// contentSecurityPolicy is sent on every page. Note bodies are rendered
// as raw HTML (a model can author them), and this renderer performs
// vault writes, so an injected script must never run: only our own
// embedded scripts load, nothing inline, no frames, no objects, and
// forms may only post back to this origin.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data: https:; connect-src 'self'; form-action 'self'; " +
	"frame-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

// Renderer serves the HTML views. Index, Search, VaultRoot, Token and Log
// must be set (New does); Writable is opt-in.
type Renderer struct {
	Index     *vault.Index
	Search    *search.Searcher
	VaultRoot string
	Token     string
	Log       *slog.Logger

	// Writable enables the renderer's only writes: Al-Mina's approve/
	// deny/defer and the dakhil->amil promotion on a human's first open.
	// Off by default — the vault must be a git repository for any write to
	// commit, and tests over the read-only fixture vault must never write.
	Writable bool

	tmpl  *template.Template
	guard *session.Guard
	csrf  string
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
		guard:     session.NewGuard(token),
		csrf:      csrfToken(token),
	}, nil
}

// csrfToken derives the form token from the login token, so it needs no
// storage and survives restarts. It is embedded only in Al-Mina's and
// the setup page's own forms; a note body cannot learn it (CSP blocks
// scripts, and pages are same-origin only), so a form injected into a
// note cannot forge a POST.
func csrfToken(token string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("null-csrf-v1"))
	return hex.EncodeToString(m.Sum(nil))
}

// Mount registers the HTML routes on r. /login is the only unauthenticated
// route; everything else requires the bearer token, via header or cookie.
func (rd *Renderer) Mount(r chi.Router) {
	r.Get("/login", rd.guard.Login)
	r.Group(func(r chi.Router) {
		r.Use(rd.guard.Require)
		r.Use(securityHeaders)
		r.Get("/", rd.handleList)
		r.Get("/n/*", rd.handleNote)
		r.Get("/s", rd.handleSearch)
		r.Get("/graph", rd.handleGraph)
		r.Get("/graph/data", rd.handleGraphData)
		r.Get("/al-mina", rd.handleMina)
		r.Post("/al-mina/act", rd.handleMinaAct)
		sub, _ := fs.Sub(staticFS, "static")
		r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(sub)))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// tierChip is one tier's count in the always-visible header.
type tierChip struct {
	Tier  string
	Count int
}

// headerData is the accumulation check shown on every page: how many
// notes sit at each tier, and how deep Al-Mina's queue is. Colour
// discriminates at fifty nodes and fails at eight hundred; a number
// scales.
func (rd *Renderer) headerData() map[string]any {
	counts := rd.Index.TierCounts()
	chips := make([]tierChip, 0, 4)
	for _, t := range []vault.Tier{vault.TierDakhil, vault.TierAmil, vault.TierThabit, vault.TierAsil} {
		chips = append(chips, tierChip{Tier: string(t), Count: counts[t]})
	}
	return map[string]any{
		"Chips": chips,
		"Mina":  len(vault.MinaQueue(rd.Index, vault.MinaStaleDays(), time.Now())),
	}
}

func (rd *Renderer) render(w http.ResponseWriter, status int, name string, data any) {
	if m, ok := data.(map[string]any); ok {
		m["Header"] = rd.headerData()
	}
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
func (rd *Renderer) handleList(w http.ResponseWriter, r *http.Request) {
	tier := vault.Tier(r.URL.Query().Get("tier"))
	if !tier.Valid() {
		tier = ""
	}
	byFolder := map[string][]*vault.Note{}
	count := 0
	for _, n := range rd.Index.All() {
		if tier != "" && n.Tier != tier {
			continue
		}
		count++
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
		"Count":  count,
		"Tier":   string(tier),
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

	promoted := rd.markOpened(r, n)
	if promoted {
		n, _ = rd.Index.Get(rel) // re-read: the tier just changed
	}

	rd.render(w, http.StatusOK, "note.html", map[string]any{
		"Title":       n.Title,
		"Note":        n,
		"Frontmatter": frontmatterRows(n.Frontmatter),
		"History":     tierHistory(n.Frontmatter),
		"Proposal":    vault.ProposalFrom(n.Frontmatter),
		"Promoted":    promoted,
		"Body":        rd.markdown(n.Body),
		"Backlinks":   rd.Index.Backlinks(n.Path),
	})
}

// markOpened promotes a dakhil note to amil when a human opens it: the
// read is exactly the evidence tier 2 claims, and the model cannot forge
// it because it counts only genuine, user-activated browser navigations.
// That means a cookie session (never the Authorization header a program
// uses) AND Sec-Fetch-User: ?1 with Sec-Fetch-Dest: document — which an
// <img>, <iframe>, script, prefetch, or meta-refresh planted inside a
// model-written note can never produce. A client that sends no Sec-Fetch
// headers at all simply never promotes; the safe default. It reports
// whether the note was promoted. Failures are logged, never shown — a
// read must not fail because a promotion did.
func (rd *Renderer) markOpened(r *http.Request, n *vault.Note) bool {
	if !rd.Writable || n.Tier != vault.TierDakhil || !session.ViaCookie(r) {
		return false
	}
	h := r.Header
	if h.Get("Sec-Fetch-User") != "?1" || h.Get("Sec-Fetch-Dest") != "document" || h.Get("Sec-Fetch-Mode") != "navigate" {
		return false
	}
	promoted, err := vault.MarkOpened(rd.VaultRoot, n.Path)
	if err != nil {
		rd.Log.Error("promote on first open", "path", n.Path, "err", err)
		return false
	}
	if promoted {
		rd.Index.Refresh(n.Path)
	}
	return promoted
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
		if k == "tier_history" {
			continue // rendered as its own list
		}
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

// historyRow is one tier_history entry, for display.
type historyRow struct{ At, From, To, Reason, By string }

// tierHistory reads a note's tier_history frontmatter, oldest first as
// stored. Entries that aren't the expected shape are skipped.
func tierHistory(fm map[string]any) []historyRow {
	raw, _ := fm["tier_history"].([]any)
	rows := make([]historyRow, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		str := func(k string) string { s, _ := m[k].(string); return s }
		rows = append(rows, historyRow{At: str("at"), From: str("from"), To: str("to"), Reason: str("reason"), By: str("by")})
	}
	return rows
}
