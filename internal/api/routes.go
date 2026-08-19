// Package api holds the HTTP layer: routing, middleware, and DTOs.
// Handlers do HTTP only; all vault logic lives in internal/vault and
// internal/search.
package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Server wires the HTTP layer to its dependencies. All fields must be set
// before Router is called; none may change afterwards.
type Server struct {
	Token        string
	MaxBodyBytes int64
	NotesIndexed func() int
	Log          *slog.Logger
}

// Router builds the chi router. It assumes the Server is fully populated:
// a nil Log or NotesIndexed will panic on first request, deliberately.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(s.requestLogger)
	r.Use(s.requireBearer) // exempts /v1/health internally

	r.Get("/v1/health", s.handleHealth)

	// /v1/notes, /v1/search, /v1/graph mount here in M3–M5.

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})
	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"notes_indexed": s.NotesIndexed(),
	})
}
