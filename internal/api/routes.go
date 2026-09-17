// Package api holds the HTTP layer: routing, middleware, and DTOs.
// Handlers do HTTP only; all vault logic lives in internal/vault and
// internal/search.
package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"null-service/internal/search"
	"null-service/internal/vault"
)

// Renderer mounts the HTML routes onto the shared router. Satisfied by
// *render.Renderer; an interface here keeps api from importing render.
type Renderer interface {
	Mount(r chi.Router)
}

// Server wires the HTTP layer to its dependencies. All fields must be set
// before Router is called; none may change afterwards. Renderer may be nil
// (API only).
type Server struct {
	Token        string
	MaxBodyBytes int64
	VaultRoot    string
	Index        *vault.Index
	Search       *search.Searcher
	Renderer     Renderer
	Log          *slog.Logger
}

// Router builds the chi router. It assumes the Server is fully populated:
// a nil Log or NotesIndexed will panic on first request, deliberately.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(s.requestLogger)

	r.Group(func(v chi.Router) {
		v.Use(s.requireBearer) // exempts /v1/health internally

		v.Get("/v1/health", s.handleHealth)
		v.Get("/v1/notes", s.handleListNotes)
		v.Get("/v1/notes/*", s.handleGetNote)
		v.Get("/v1/search", s.handleSearch)
		v.Get("/v1/graph", s.handleGraph)
		v.Get("/mina", s.handleMina)
	})

	if s.Renderer != nil {
		s.Renderer.Mount(r)
	}

	// Unknown routes reveal nothing without a token: 401 first, 404 after.
	r.NotFound(s.requireBearer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})).ServeHTTP)
	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"notes_indexed": s.Index.Len(),
	})
}
