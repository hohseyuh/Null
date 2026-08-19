package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"null-service/internal/vault"
)

// noteSummary is the metadata-only shape /notes returns. It must never
// grow a body field — that is non-negotiable #3.
type noteSummary struct {
	Path          string         `json:"path"`
	Title         string         `json:"title"`
	Tags          []string       `json:"tags"`
	Frontmatter   map[string]any `json:"frontmatter"`
	UpdatedAt     time.Time      `json:"updated_at"`
	SizeBytes     int64          `json:"size_bytes"`
	OutlinkCount  int            `json:"outlink_count"`
	BacklinkCount int            `json:"backlink_count"`
}

// handleListNotes serves GET /v1/notes: filterable, cursor-paginated,
// metadata only.
func (s *Server) handleListNotes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		limit = min(n, 200)
	}

	sortBy := q.Get("sort")
	if sortBy == "" {
		sortBy = "updated"
	}
	if sortBy != "updated" && sortBy != "path" {
		writeError(w, http.StatusBadRequest, "bad_request", "sort must be `path` or `updated`")
		return
	}

	var updatedAfter time.Time
	if v := q.Get("updated_after"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "updated_after must be ISO 8601 (RFC 3339)")
			return
		}
		updatedAfter = t
	}

	folder := strings.TrimSuffix(q.Get("folder"), "/")
	tags := q["tag"]

	notes := s.Index.All()
	filtered := notes[:0:0]
	for _, n := range notes {
		if folder != "" && !strings.HasPrefix(n.Path, folder+"/") {
			continue
		}
		if !updatedAfter.IsZero() && !n.UpdatedAt.After(updatedAfter) {
			continue
		}
		if !hasAllTags(n.Tags, tags) {
			continue
		}
		filtered = append(filtered, n)
	}

	// All() is path-sorted already; updated sorts desc with path tiebreak
	// so cursor pagination stays stable between requests.
	if sortBy == "updated" {
		sort.SliceStable(filtered, func(i, j int) bool {
			if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
				return filtered[i].UpdatedAt.After(filtered[j].UpdatedAt)
			}
			return filtered[i].Path < filtered[j].Path
		})
	}

	// The cursor is the last path of the previous page: skip through it.
	if cursor := q.Get("cursor"); cursor != "" {
		pos := slices.IndexFunc(filtered, func(n *vault.Note) bool { return n.Path == cursor })
		if pos < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "unknown cursor")
			return
		}
		filtered = filtered[pos+1:]
	}

	more := len(filtered) > limit
	if more {
		filtered = filtered[:limit]
	}

	out := make([]noteSummary, 0, len(filtered))
	for _, n := range filtered {
		out = append(out, s.summarize(n))
	}
	resp := map[string]any{"notes": out}
	if more {
		resp["next_cursor"] = out[len(out)-1].Path
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) summarize(n *vault.Note) noteSummary {
	tags := n.Tags
	if tags == nil {
		tags = []string{}
	}
	return noteSummary{
		Path:          n.Path,
		Title:         n.Title,
		Tags:          tags,
		Frontmatter:   n.Frontmatter,
		UpdatedAt:     n.UpdatedAt,
		SizeBytes:     n.SizeBytes,
		OutlinkCount:  len(n.Outlinks),
		BacklinkCount: len(s.Index.Backlinks(n.Path)),
	}
}

// hasAllTags reports whether note tags contain every wanted tag (AND).
func hasAllTags(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// noteResponse is the full-note shape of GET /v1/notes/{path} — the only
// route in the API allowed to carry a body.
type noteResponse struct {
	Path        string          `json:"path"`
	Frontmatter map[string]any  `json:"frontmatter"`
	Body        *string         `json:"body,omitempty"`
	Headings    []vault.Heading `json:"headings"`
	Outlinks    []string        `json:"outlinks,omitempty"`
	Backlinks   []string        `json:"backlinks,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// handleGetNote serves GET /v1/notes/{path...}.
func (s *Server) handleGetNote(w http.ResponseWriter, r *http.Request) {
	rel, err := vault.SafeRequestPath(s.VaultRoot, strings.TrimPrefix(r.URL.Path, "/v1/notes/"))
	if err != nil {
		if errors.Is(err, vault.ErrPathHidden) {
			writeError(w, http.StatusNotFound, "not_found", "no such note")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid note path")
		return
	}

	include := map[string]bool{"body": true}
	if v := r.URL.Query().Get("include"); v != "" {
		include = map[string]bool{}
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			switch part {
			case "body", "outlinks", "backlinks":
				include[part] = true
			default:
				writeError(w, http.StatusBadRequest, "bad_request",
					fmt.Sprintf("unknown include value %q; valid: body, outlinks, backlinks", part))
				return
			}
		}
	}

	n, ok := s.Index.Get(rel)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such note")
		return
	}

	resp := noteResponse{
		Path:        n.Path,
		Frontmatter: n.Frontmatter,
		Headings:    n.Headings,
		UpdatedAt:   n.UpdatedAt,
	}
	if resp.Headings == nil {
		resp.Headings = []vault.Heading{}
	}

	if include["body"] || r.URL.Query().Has("section") {
		body := n.Body
		if section := r.URL.Query().Get("section"); section != "" {
			sliced, ok := n.Section(section)
			if !ok {
				writeError(w, http.StatusNotFound, "unknown_section",
					fmt.Sprintf("no heading %q in %s", section, n.Path))
				return
			}
			body = sliced
		}
		if int64(len(body)) > s.MaxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("body is %d bytes, cap is %d; fetch a slice with ?section=<heading>", len(body), s.MaxBodyBytes))
			return
		}
		resp.Body = &body
	}

	if include["outlinks"] {
		out := []string{}
		for _, l := range n.Outlinks {
			if l.Resolved && !slices.Contains(out, l.Path) {
				out = append(out, l.Path)
			}
		}
		resp.Outlinks = out
	}
	if include["backlinks"] {
		back := []string{}
		for _, e := range s.Index.Backlinks(n.Path) {
			if !slices.Contains(back, e.From) {
				back = append(back, e.From)
			}
		}
		resp.Backlinks = back
	}

	writeJSON(w, http.StatusOK, resp)
}
