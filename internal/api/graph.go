package api

import (
	"errors"
	"net/http"
	"strconv"

	"null-service/internal/vault"
)

// graphEdge is the wire shape of one edge: from, to, and the line the
// link was written on.
type graphEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Context string `json:"context"`
}

// handleGraph serves GET /v1/graph: the BFS neighborhood of one note.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	rel, err := vault.SafeRequestPath(s.VaultRoot, q.Get("path"))
	if err != nil {
		if errors.Is(err, vault.ErrPathHidden) {
			writeError(w, http.StatusNotFound, "not_found", "no such note")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "path is required and must be a valid vault path")
		return
	}
	if _, ok := s.Index.Get(rel); !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such note")
		return
	}

	depth := 1
	if v := q.Get("depth"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3 {
			writeError(w, http.StatusBadRequest, "bad_request", "depth must be an integer between 1 and 3")
			return
		}
		depth = n
	}

	direction := q.Get("direction")
	if direction == "" {
		direction = "both"
	}
	if direction != "out" && direction != "in" && direction != "both" {
		writeError(w, http.StatusBadRequest, "bad_request", "direction must be `out`, `in`, or `both`")
		return
	}

	g := s.Index.Graph(rel, depth, direction)
	edges := make([]graphEdge, 0, len(g.Edges))
	for _, e := range g.Edges {
		edges = append(edges, graphEdge{From: e.From, To: e.To, Context: e.Context})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"root":  g.Root,
		"nodes": g.Nodes,
		"edges": edges,
	})
}
