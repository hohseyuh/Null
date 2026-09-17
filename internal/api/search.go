package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"null-service/internal/search"
	"null-service/internal/vault"
)

// searchResult is one hit in a /search response: snippets, never bodies.
// Score is weighted by tier (spec/tiers.md) — settled knowledge outranks
// raw capture without excluding it from results.
type searchResult struct {
	Path    string         `json:"path"`
	Title   string         `json:"title"`
	Tier    string         `json:"tier"`
	Score   float64        `json:"score"`
	Matches []search.Match `json:"matches"`
}

// handleSearch serves GET /v1/search. Body matches come from ripgrep;
// title matches from the index; folder/tag filters intersect with the
// index after rg returns, so search can never surface a note the index
// does not know.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}

	in := r.URL.Query().Get("in")
	if in == "" {
		in = "body"
	}
	if in != "body" && in != "title" && in != "both" {
		writeError(w, http.StatusBadRequest, "bad_request", "in must be `body`, `title`, or `both`")
		return
	}

	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		limit = min(n, 50)
	}

	var tier vault.Tier
	if v := r.URL.Query().Get("tier"); v != "" {
		tier = vault.Tier(v)
		if !tier.Valid() {
			writeError(w, http.StatusBadRequest, "bad_request", "tier must be one of dakhil, amil, thabit, asil")
			return
		}
	}

	folder := strings.TrimSuffix(r.URL.Query().Get("folder"), "/")
	tags := r.URL.Query()["tag"]
	visible := func(path string) bool {
		n, ok := s.Index.Get(path)
		if !ok {
			return false
		}
		if folder != "" && !strings.HasPrefix(path, folder+"/") {
			return false
		}
		if tier != "" && n.Tier != tier {
			return false
		}
		return hasAllTags(n.Tags, tags)
	}

	hits := map[string]*searchResult{}

	if in == "body" || in == "both" {
		results, err := s.Search.Search(r.Context(), q)
		if err != nil {
			s.Log.Error("search failed", "q", q, "err", err)
			writeError(w, http.StatusInternalServerError, "search_failed", "search backend error")
			return
		}
		maxCount := 0
		for _, res := range results {
			if len(res.Matches) > maxCount {
				maxCount = len(res.Matches)
			}
		}
		for _, res := range results {
			if !visible(res.Path) {
				continue
			}
			n, _ := s.Index.Get(res.Path)
			hits[res.Path] = &searchResult{
				Path:    res.Path,
				Title:   n.Title,
				Tier:    string(n.Tier),
				Score:   (float64(len(res.Matches)) / float64(maxCount)) * n.Tier.SearchWeight(),
				Matches: res.Matches,
			}
		}
	}

	if in == "title" || in == "both" {
		folded := search.Fold(q)
		for _, n := range s.Index.All() {
			if !strings.Contains(search.Fold(n.Title), folded) || !visible(n.Path) {
				continue
			}
			score := 1.0 * n.Tier.SearchWeight()
			if h, ok := hits[n.Path]; ok {
				h.Score = score // matched both title and body
				continue
			}
			hits[n.Path] = &searchResult{
				Path:    n.Path,
				Title:   n.Title,
				Tier:    string(n.Tier),
				Score:   score,
				Matches: []search.Match{},
			}
		}
	}

	out := make([]searchResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > limit {
		out = out[:limit]
	}

	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}
