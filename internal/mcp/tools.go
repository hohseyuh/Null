// Package mcp exposes the vault as Model Context Protocol tools:
// list_notes, get_note, search_notes, get_graph — the same four
// operations as the JSON API, reshaped for a language model working
// inside a token budget instead of a browser.
//
// The non-negotiables from CLAUDE.md apply here exactly as they do to
// internal/api: list_notes and search_notes never return a body (here
// that is enforced by the type system — neither Out struct has a Body
// field to leak), every path argument passes through
// vault.SafeRequestPath, and nothing in this package writes to the vault.
//
// This file is protocol-agnostic on purpose: it imports only vault and
// search, never the MCP SDK. server.go is the thin adapter that wires
// these methods to SDK tool handlers, the same separation api/notes.go
// keeps from api/routes.go.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"null-service/internal/search"
	"null-service/internal/vault"
)

// Tools holds the dependencies behind every tool call. It mirrors
// api.Server in shape but speaks Go in, Go out — no HTTP.
type Tools struct {
	Index        *vault.Index
	Search       *search.Searcher
	VaultRoot    string
	MaxBodyBytes int64
	Log          *slog.Logger
}

func (t *Tools) logResult(tool string, n int) {
	if t.Log != nil {
		t.Log.Info("mcp tool call", "tool", tool, "result_count", n)
	}
}

// hasAllTags reports whether have contains every tag in want (AND).
func hasAllTags(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// ListNotesIn is the input to list_notes.
type ListNotesIn struct {
	Folder       string   `json:"folder,omitempty" jsonschema:"prefix filter on the note path, e.g. 'engineering/'"`
	Tags         []string `json:"tags,omitempty" jsonschema:"every listed tag must be present on the note (AND)"`
	UpdatedAfter string   `json:"updated_after,omitempty" jsonschema:"ISO 8601 timestamp; only notes updated after this"`
	Limit        int      `json:"limit,omitempty" jsonschema:"1-200, default 50"`
	Cursor       string   `json:"cursor,omitempty" jsonschema:"opaque, taken from a previous call's next_cursor"`
	Sort         string   `json:"sort,omitempty" jsonschema:"'path' or 'updated' (default 'updated', descending)"`
}

// NoteSummary is the metadata-only shape list_notes and search_notes'
// title mode return. It has no Body field, deliberately — the type
// itself is the guarantee.
type NoteSummary struct {
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	Tags        []string       `json:"tags"`
	Frontmatter map[string]any `json:"frontmatter"`
	UpdatedAt   time.Time      `json:"updated_at"`
	SizeBytes   int64          `json:"size_bytes"`
	// ApproxTokens is a rough size_bytes/4 heuristic (English prose
	// averages ~4 bytes/token). Use it to budget get_note calls before
	// making them, not as an exact count.
	ApproxTokens  int64 `json:"approx_tokens"`
	OutlinkCount  int   `json:"outlink_count"`
	BacklinkCount int   `json:"backlink_count"`
}

// ListNotesOut is the output of list_notes.
type ListNotesOut struct {
	Notes      []NoteSummary `json:"notes"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// ListNotes mirrors GET /v1/notes: metadata only, cursor-paginated.
// Filtering, sorting, and pagination intentionally follow the same
// contract as internal/api/notes.go's handleListNotes — same guarantee,
// different protocol; that logic is not shared beyond this comment
// because each handler layer owns its own glue by convention.
func (t *Tools) ListNotes(_ context.Context, in ListNotesIn) (ListNotesOut, error) {
	limit := 50
	if in.Limit != 0 {
		if in.Limit < 1 {
			return ListNotesOut{}, fmt.Errorf("limit must be a positive integer")
		}
		limit = min(in.Limit, 200)
	}

	sortBy := in.Sort
	if sortBy == "" {
		sortBy = "updated"
	}
	if sortBy != "updated" && sortBy != "path" {
		return ListNotesOut{}, fmt.Errorf("sort must be 'path' or 'updated'")
	}

	var updatedAfter time.Time
	if in.UpdatedAfter != "" {
		parsed, err := time.Parse(time.RFC3339, in.UpdatedAfter)
		if err != nil {
			return ListNotesOut{}, fmt.Errorf("updated_after must be ISO 8601 (RFC 3339): %w", err)
		}
		updatedAfter = parsed
	}

	folder := strings.TrimSuffix(in.Folder, "/")

	notes := t.Index.All()
	filtered := notes[:0:0]
	for _, n := range notes {
		if folder != "" && !strings.HasPrefix(n.Path, folder+"/") {
			continue
		}
		if !updatedAfter.IsZero() && !n.UpdatedAt.After(updatedAfter) {
			continue
		}
		if !hasAllTags(n.Tags, in.Tags) {
			continue
		}
		filtered = append(filtered, n)
	}

	if sortBy == "updated" {
		sort.SliceStable(filtered, func(i, j int) bool {
			if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
				return filtered[i].UpdatedAt.After(filtered[j].UpdatedAt)
			}
			return filtered[i].Path < filtered[j].Path
		})
	}

	if in.Cursor != "" {
		pos := slices.IndexFunc(filtered, func(n *vault.Note) bool { return n.Path == in.Cursor })
		if pos < 0 {
			return ListNotesOut{}, fmt.Errorf("unknown cursor %q", in.Cursor)
		}
		filtered = filtered[pos+1:]
	}

	more := len(filtered) > limit
	if more {
		filtered = filtered[:limit]
	}

	out := ListNotesOut{Notes: make([]NoteSummary, 0, len(filtered))}
	for _, n := range filtered {
		out.Notes = append(out.Notes, t.summarize(n))
	}
	if more {
		out.NextCursor = out.Notes[len(out.Notes)-1].Path
	}
	t.logResult("list_notes", len(out.Notes))
	return out, nil
}

func (t *Tools) summarize(n *vault.Note) NoteSummary {
	tags := n.Tags
	if tags == nil {
		tags = []string{}
	}
	return NoteSummary{
		Path:          n.Path,
		Title:         n.Title,
		Tags:          tags,
		Frontmatter:   n.Frontmatter,
		UpdatedAt:     n.UpdatedAt,
		SizeBytes:     n.SizeBytes,
		ApproxTokens:  n.SizeBytes / 4,
		OutlinkCount:  len(n.Outlinks),
		BacklinkCount: len(t.Index.Backlinks(n.Path)),
	}
}

// GetNoteIn is the input to get_note.
type GetNoteIn struct {
	Path string `json:"path" jsonschema:"vault-relative path, e.g. 'engineering/basim/soul.md'"`
	// Section, when set, returns only that heading through the next
	// heading of the same or higher level — pull one clause instead of
	// a whole note. Prefer this whenever a note's approx_tokens (from
	// list_notes or search_notes) looked large.
	Section string   `json:"section,omitempty" jsonschema:"exact heading text; slices the note to just that section"`
	Include []string `json:"include,omitempty" jsonschema:"any of 'body' (default), 'outlinks', 'backlinks'"`
}

// GetNoteOut is the output of get_note — the only tool that returns a
// body.
type GetNoteOut struct {
	Path        string          `json:"path"`
	Frontmatter map[string]any  `json:"frontmatter"`
	Body        string          `json:"body,omitempty"`
	Headings    []vault.Heading `json:"headings"`
	Outlinks    []string        `json:"outlinks,omitempty"`
	Backlinks   []string        `json:"backlinks,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// GetNote mirrors GET /v1/notes/{path}. Bodies are capped at
// MaxBodyBytes; past that it errors and points the caller at section,
// the same contract as the HTTP API's 413.
func (t *Tools) GetNote(_ context.Context, in GetNoteIn) (GetNoteOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return GetNoteOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}

	include := map[string]bool{"body": true}
	if len(in.Include) > 0 {
		include = map[string]bool{}
		for _, part := range in.Include {
			switch part {
			case "body", "outlinks", "backlinks":
				include[part] = true
			default:
				return GetNoteOut{}, fmt.Errorf("unknown include value %q; valid: body, outlinks, backlinks", part)
			}
		}
	}

	n, ok := t.Index.Get(rel)
	if !ok {
		return GetNoteOut{}, fmt.Errorf("no such note: %s", rel)
	}

	out := GetNoteOut{
		Path:        n.Path,
		Frontmatter: n.Frontmatter,
		Headings:    n.Headings,
		UpdatedAt:   n.UpdatedAt,
	}
	if out.Headings == nil {
		out.Headings = []vault.Heading{}
	}

	if include["body"] || in.Section != "" {
		body := n.Body
		if in.Section != "" {
			sliced, ok := n.Section(in.Section)
			if !ok {
				return GetNoteOut{}, fmt.Errorf("no heading %q in %s", in.Section, n.Path)
			}
			body = sliced
		}
		if int64(len(body)) > t.MaxBodyBytes {
			return GetNoteOut{}, fmt.Errorf(
				"body is %d bytes, cap is %d; fetch a slice with section instead of the whole note",
				len(body), t.MaxBodyBytes)
		}
		out.Body = body
	}

	if include["outlinks"] {
		links := []string{}
		for _, l := range n.Outlinks {
			if l.Resolved && !slices.Contains(links, l.Path) {
				links = append(links, l.Path)
			}
		}
		out.Outlinks = links
	}
	if include["backlinks"] {
		back := []string{}
		for _, e := range t.Index.Backlinks(n.Path) {
			if !slices.Contains(back, e.From) {
				back = append(back, e.From)
			}
		}
		out.Backlinks = back
	}

	t.logResult("get_note", len(out.Body))
	return out, nil
}

// SearchNotesIn is the input to search_notes.
type SearchNotesIn struct {
	Query  string   `json:"query" jsonschema:"required; case- and diacritic-insensitive substring"`
	In     string   `json:"in,omitempty" jsonschema:"'body' (default), 'title', or 'both'"`
	Folder string   `json:"folder,omitempty"`
	Tags   []string `json:"tags,omitempty" jsonschema:"every listed tag must be present (AND)"`
	Limit  int      `json:"limit,omitempty" jsonschema:"1-50, default 20"`
}

// SearchHit is one note's matches within a search_notes result.
type SearchHit struct {
	Path    string         `json:"path"`
	Title   string         `json:"title"`
	Score   float64        `json:"score"`
	Matches []search.Match `json:"matches"`
}

// SearchNotesOut is the output of search_notes — snippets, never bodies.
type SearchNotesOut struct {
	Results []SearchHit `json:"results"`
}

// SearchNotes mirrors GET /v1/search. Use the returned path with
// get_note, ideally with section set to the heading nearest the match,
// rather than fetching the whole note.
func (t *Tools) SearchNotes(ctx context.Context, in SearchNotesIn) (SearchNotesOut, error) {
	if in.Query == "" {
		return SearchNotesOut{}, fmt.Errorf("query is required")
	}
	mode := in.In
	if mode == "" {
		mode = "body"
	}
	if mode != "body" && mode != "title" && mode != "both" {
		return SearchNotesOut{}, fmt.Errorf("in must be 'body', 'title', or 'both'")
	}
	limit := 20
	if in.Limit != 0 {
		if in.Limit < 1 {
			return SearchNotesOut{}, fmt.Errorf("limit must be a positive integer")
		}
		limit = min(in.Limit, 50)
	}

	folder := strings.TrimSuffix(in.Folder, "/")
	visible := func(path string) bool {
		n, ok := t.Index.Get(path)
		if !ok {
			return false
		}
		if folder != "" && !strings.HasPrefix(path, folder+"/") {
			return false
		}
		return hasAllTags(n.Tags, in.Tags)
	}

	hits := map[string]*SearchHit{}

	if mode == "body" || mode == "both" {
		results, err := t.Search.Search(ctx, in.Query)
		if err != nil {
			return SearchNotesOut{}, fmt.Errorf("search backend: %w", err)
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
			n, _ := t.Index.Get(res.Path)
			hits[res.Path] = &SearchHit{
				Path: res.Path, Title: n.Title,
				Score: float64(len(res.Matches)) / float64(maxCount), Matches: res.Matches,
			}
		}
	}

	if mode == "title" || mode == "both" {
		folded := search.Fold(in.Query)
		for _, n := range t.Index.All() {
			if !strings.Contains(search.Fold(n.Title), folded) || !visible(n.Path) {
				continue
			}
			if h, ok := hits[n.Path]; ok {
				h.Score = 1.0
				continue
			}
			hits[n.Path] = &SearchHit{Path: n.Path, Title: n.Title, Score: 1.0, Matches: []search.Match{}}
		}
	}

	out := SearchNotesOut{Results: make([]SearchHit, 0, len(hits))}
	for _, h := range hits {
		out.Results = append(out.Results, *h)
	}
	sort.Slice(out.Results, func(i, j int) bool {
		if out.Results[i].Score != out.Results[j].Score {
			return out.Results[i].Score > out.Results[j].Score
		}
		return out.Results[i].Path < out.Results[j].Path
	})
	if len(out.Results) > limit {
		out.Results = out.Results[:limit]
	}
	t.logResult("search_notes", len(out.Results))
	return out, nil
}

// GetGraphIn is the input to get_graph.
type GetGraphIn struct {
	Path      string `json:"path" jsonschema:"required; the anchor note"`
	Depth     int    `json:"depth,omitempty" jsonschema:"1-3, default 1"`
	Direction string `json:"direction,omitempty" jsonschema:"'out', 'in', or 'both' (default)"`
}

// GraphNodeOut is one note in a get_graph result.
type GraphNodeOut struct {
	Path     string `json:"path"`
	Title    string `json:"title"`
	Distance int    `json:"distance"`
}

// GraphEdgeOut is one link in a get_graph result, carrying the line it
// was written on.
type GraphEdgeOut struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Context string `json:"context"`
}

// GetGraphOut is the output of get_graph.
type GetGraphOut struct {
	Root  string         `json:"root"`
	Nodes []GraphNodeOut `json:"nodes"`
	Edges []GraphEdgeOut `json:"edges"`
}

// GetGraph mirrors GET /v1/graph: the BFS neighborhood of one note, with
// the line each wikilink was written on — why two notes are connected,
// not just that they are. Cheaper than fetching notes to rediscover
// their relationships.
func (t *Tools) GetGraph(_ context.Context, in GetGraphIn) (GetGraphOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return GetGraphOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}
	if _, ok := t.Index.Get(rel); !ok {
		return GetGraphOut{}, fmt.Errorf("no such note: %s", rel)
	}

	depth := 1
	if in.Depth != 0 {
		if in.Depth < 1 || in.Depth > 3 {
			return GetGraphOut{}, fmt.Errorf("depth must be between 1 and 3")
		}
		depth = in.Depth
	}

	direction := in.Direction
	if direction == "" {
		direction = "both"
	}
	if direction != "out" && direction != "in" && direction != "both" {
		return GetGraphOut{}, fmt.Errorf("direction must be 'out', 'in', or 'both'")
	}

	g := t.Index.Graph(rel, depth, direction)
	out := GetGraphOut{
		Root:  g.Root,
		Nodes: make([]GraphNodeOut, 0, len(g.Nodes)),
		Edges: make([]GraphEdgeOut, 0, len(g.Edges)),
	}
	for _, n := range g.Nodes {
		out.Nodes = append(out.Nodes, GraphNodeOut{Path: n.Path, Title: n.Title, Distance: n.Distance})
	}
	for _, e := range g.Edges {
		out.Edges = append(out.Edges, GraphEdgeOut{From: e.From, To: e.To, Context: e.Context})
	}
	t.logResult("get_graph", len(out.Nodes))
	return out, nil
}
