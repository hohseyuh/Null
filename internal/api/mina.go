package api

import (
	"net/http"
	"time"

	"null-service/internal/vault"
)

// handleMina serves GET /mina: the Al-Mina queue — every note with an
// outstanding proposal, plus dakhil notes stale enough for a sweep.
// Read-only: it is a query over frontmatter the index already holds.
// Approving, denying, and deferring an entry is a human action taken in
// the renderer's Al-Mina screen (/al-mina), never through this API.
func (s *Server) handleMina(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": vault.MinaQueue(s.Index, vault.MinaStaleDays(), time.Now()),
	})
}
