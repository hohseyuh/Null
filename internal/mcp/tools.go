// Package mcp exposes the vault as Model Context Protocol tools:
// list_notes, get_note, search_notes, get_graph, find_relatives,
// get_links, find_path, create_note, write_note, delete_note, tier_get,
// tier_set, and tier_propose.
//
// list_notes and search_notes never return a body (enforced by the type
// system — neither Out struct has a Body field to leak), every path
// argument passes through vault.SafeRequestPath, and every write ends in
// exactly one git commit — see vault/write.go's package doc for why that
// is the safety model instead of a staging area.
//
// There is deliberately no git push or git commit tool here at all — see
// spec/tiers.md's "One door". The server commits on write; nothing in
// this codebase ever sends anything to a remote.
//
// Every note carries a tier (spec/tiers.md): dakhil, amil, thabit, or
// asil, ordinal, server-owned. The model's only write path to the field
// is tier_set, which can only lower it (R1), and tier_propose, which
// only records an ask for the user to act on in Al-Mina. Promotion is
// never reachable from this package.
//
// This file is protocol-agnostic on purpose: it imports only vault and
// search, never the MCP SDK. server.go is the thin adapter that wires
// these methods to SDK tool handlers, the same separation api/notes.go
// keeps from api/routes.go.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
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
	Folder        string   `json:"folder,omitempty" jsonschema:"prefix filter on the note path, e.g. 'engineering/'"`
	Tags          []string `json:"tags,omitempty" jsonschema:"every listed tag must be present on the note (AND)"`
	Tier          string   `json:"tier,omitempty" jsonschema:"one of dakhil, amil, thabit, asil — exact match"`
	UpdatedAfter  string   `json:"updated_after,omitempty" jsonschema:"ISO 8601 timestamp; only notes updated after this"`
	UpdatedBefore string   `json:"updated_before,omitempty" jsonschema:"ISO 8601 timestamp; only notes updated before this — e.g. a staleness sweep over old dakhil notes"`
	Limit         int      `json:"limit,omitempty" jsonschema:"1-200, default 50"`
	Cursor        string   `json:"cursor,omitempty" jsonschema:"opaque, taken from a previous call's next_cursor"`
	Sort          string   `json:"sort,omitempty" jsonschema:"'path' or 'updated' (default 'updated', descending)"`
}

// NoteSummary is the metadata-only shape list_notes and search_notes'
// title mode return. It has no Body field, deliberately — the type
// itself is the guarantee.
type NoteSummary struct {
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	Tags        []string       `json:"tags"`
	Tier        string         `json:"tier"`
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

	var updatedAfter, updatedBefore time.Time
	if in.UpdatedAfter != "" {
		parsed, err := time.Parse(time.RFC3339, in.UpdatedAfter)
		if err != nil {
			return ListNotesOut{}, fmt.Errorf("updated_after must be ISO 8601 (RFC 3339): %w", err)
		}
		updatedAfter = parsed
	}
	if in.UpdatedBefore != "" {
		parsed, err := time.Parse(time.RFC3339, in.UpdatedBefore)
		if err != nil {
			return ListNotesOut{}, fmt.Errorf("updated_before must be ISO 8601 (RFC 3339): %w", err)
		}
		updatedBefore = parsed
	}
	var tier vault.Tier
	if in.Tier != "" {
		tier = vault.Tier(in.Tier)
		if !tier.Valid() {
			return ListNotesOut{}, fmt.Errorf("unknown tier %q; must be one of dakhil, amil, thabit, asil", in.Tier)
		}
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
		if !updatedBefore.IsZero() && !n.UpdatedAt.Before(updatedBefore) {
			continue
		}
		if tier != "" && n.Tier != tier {
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
		Tier:          string(n.Tier),
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
	Title       string          `json:"title"`
	Tier        string          `json:"tier"`
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
		Title:       n.Title,
		Tier:        string(n.Tier),
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
	Tier   string   `json:"tier,omitempty" jsonschema:"one of dakhil, amil, thabit, asil — exact match"`
	Limit  int      `json:"limit,omitempty" jsonschema:"1-50, default 20"`
}

// SearchHit is one note's matches within a search_notes result. Score is
// weighted by tier (spec/tiers.md) — settled knowledge outranks raw
// capture without excluding it from results.
type SearchHit struct {
	Path    string         `json:"path"`
	Title   string         `json:"title"`
	Tier    string         `json:"tier"`
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

	var tier vault.Tier
	if in.Tier != "" {
		tier = vault.Tier(in.Tier)
		if !tier.Valid() {
			return SearchNotesOut{}, fmt.Errorf("unknown tier %q; must be one of dakhil, amil, thabit, asil", in.Tier)
		}
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
		if tier != "" && n.Tier != tier {
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
			score := (float64(len(res.Matches)) / float64(maxCount)) * n.Tier.SearchWeight()
			hits[res.Path] = &SearchHit{
				Path: res.Path, Title: n.Title, Tier: string(n.Tier),
				Score: score, Matches: res.Matches,
			}
		}
	}

	if mode == "title" || mode == "both" {
		folded := search.Fold(in.Query)
		for _, n := range t.Index.All() {
			if !strings.Contains(search.Fold(n.Title), folded) || !visible(n.Path) {
				continue
			}
			score := 1.0 * n.Tier.SearchWeight()
			if h, ok := hits[n.Path]; ok {
				h.Score = score
				continue
			}
			hits[n.Path] = &SearchHit{Path: n.Path, Title: n.Title, Tier: string(n.Tier), Score: score, Matches: []search.Match{}}
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
	Tier     string `json:"tier"`
}

// GraphEdgeOut is one link in a get_graph result, carrying the line it
// was written on and the lower of its two endpoints' tiers — an edge is
// only as trustworthy as its weaker end.
type GraphEdgeOut struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Context string `json:"context"`
	Tier    string `json:"tier"`
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
		out.Nodes = append(out.Nodes, GraphNodeOut{Path: n.Path, Title: n.Title, Distance: n.Distance, Tier: string(n.Tier)})
	}
	for _, e := range g.Edges {
		out.Edges = append(out.Edges, GraphEdgeOut{From: e.From, To: e.To, Context: e.Context, Tier: string(e.Tier)})
	}
	t.logResult("get_graph", len(out.Nodes))
	return out, nil
}

// FindRelativesIn is the input to find_relatives.
type FindRelativesIn struct {
	Path  string `json:"path" jsonschema:"required; the note to find relatives of"`
	By    string `json:"by,omitempty" jsonschema:"'folder', 'tags', or 'both' (default) — what counts as related"`
	Limit int    `json:"limit,omitempty" jsonschema:"1-100, default 20"`
}

// Relative is one note related to the queried one by folder and/or tags —
// not by links. get_links and get_graph cover the linked sense of
// "related"; this one is metadata-based.
type Relative struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	SameFolder bool     `json:"same_folder"`
	SharedTags []string `json:"shared_tags,omitempty"`
}

// FindRelativesOut is the output of find_relatives.
type FindRelativesOut struct {
	Path      string     `json:"path"`
	Relatives []Relative `json:"relatives"`
}

// FindRelatives finds notes related to one note by folder and/or shared
// tags — organizational or topical proximity, distinct from the wikilink
// graph get_graph and get_links traverse. Two notes can be relatives
// with zero links between them, and two linked notes need not be
// relatives.
func (t *Tools) FindRelatives(_ context.Context, in FindRelativesIn) (FindRelativesOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return FindRelativesOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}
	n, ok := t.Index.Get(rel)
	if !ok {
		return FindRelativesOut{}, fmt.Errorf("no such note: %s", rel)
	}

	by := in.By
	if by == "" {
		by = "both"
	}
	if by != "folder" && by != "tags" && by != "both" {
		return FindRelativesOut{}, fmt.Errorf("by must be 'folder', 'tags', or 'both'")
	}
	limit := 20
	if in.Limit != 0 {
		if in.Limit < 1 {
			return FindRelativesOut{}, fmt.Errorf("limit must be a positive integer")
		}
		limit = min(in.Limit, 100)
	}

	folder := path.Dir(n.Path)
	var relatives []Relative
	for _, o := range t.Index.All() {
		if o.Path == n.Path {
			continue
		}
		sameFolder := path.Dir(o.Path) == folder
		var shared []string
		for _, tag := range o.Tags {
			if slices.Contains(n.Tags, tag) {
				shared = append(shared, tag)
			}
		}
		match := false
		switch by {
		case "folder":
			match = sameFolder
		case "tags":
			match = len(shared) > 0
		case "both":
			match = sameFolder || len(shared) > 0
		}
		if !match {
			continue
		}
		relatives = append(relatives, Relative{
			Path: o.Path, Title: o.Title,
			SameFolder: sameFolder, SharedTags: shared,
		})
	}

	sort.Slice(relatives, func(i, j int) bool {
		if len(relatives[i].SharedTags) != len(relatives[j].SharedTags) {
			return len(relatives[i].SharedTags) > len(relatives[j].SharedTags)
		}
		if relatives[i].SameFolder != relatives[j].SameFolder {
			return relatives[i].SameFolder // same-folder sorts first among ties
		}
		return relatives[i].Path < relatives[j].Path
	})
	if len(relatives) > limit {
		relatives = relatives[:limit]
	}
	if relatives == nil {
		relatives = []Relative{}
	}

	t.logResult("find_relatives", len(relatives))
	return FindRelativesOut{Path: rel, Relatives: relatives}, nil
}

// GetLinksIn is the input to get_links.
type GetLinksIn struct {
	Path string `json:"path" jsonschema:"required; the note to list links for"`
}

// LinkedNote is one end of a link, in or out, with the context line the
// wikilink was written on.
type LinkedNote struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Context string `json:"context"`
}

// GetLinksOut is the output of get_links — one note's direct connections,
// both directions, in a single call. Equivalent to get_graph(path,
// depth=1, direction=both) split into its two directions; use get_graph
// instead for anything past one hop.
type GetLinksOut struct {
	Path      string       `json:"path"`
	Outlinks  []LinkedNote `json:"outlinks"`
	Backlinks []LinkedNote `json:"backlinks"`
}

// GetLinks mirrors get_graph at depth 1 but returns outlinks and
// backlinks as two separate, directly labeled lists instead of one
// merged node/edge set — cheaper to read when direction is what matters
// and depth doesn't.
func (t *Tools) GetLinks(_ context.Context, in GetLinksIn) (GetLinksOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return GetLinksOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}
	if _, ok := t.Index.Get(rel); !ok {
		return GetLinksOut{}, fmt.Errorf("no such note: %s", rel)
	}

	out := GetLinksOut{Path: rel, Outlinks: []LinkedNote{}, Backlinks: []LinkedNote{}}
	for _, e := range t.Index.Outlinks(rel) {
		ln := LinkedNote{Path: e.To, Context: e.Context}
		if n, ok := t.Index.Get(e.To); ok {
			ln.Title = n.Title
		}
		out.Outlinks = append(out.Outlinks, ln)
	}
	for _, e := range t.Index.Backlinks(rel) {
		ln := LinkedNote{Path: e.From, Context: e.Context}
		if n, ok := t.Index.Get(e.From); ok {
			ln.Title = n.Title
		}
		out.Backlinks = append(out.Backlinks, ln)
	}

	t.logResult("get_links", len(out.Outlinks)+len(out.Backlinks))
	return out, nil
}

// FindPathIn is the input to find_path.
type FindPathIn struct {
	From      string `json:"from" jsonschema:"required; starting note"`
	To        string `json:"to" jsonschema:"required; target note"`
	Depth     int    `json:"depth,omitempty" jsonschema:"max hops to search, 1-6, default 4"`
	Direction string `json:"direction,omitempty" jsonschema:"'out', 'in', or 'both' (default) — link directions to follow while searching"`
}

// PathStep is one note along a found path. Via is the context line of
// the link taken to reach this step from the previous one; empty on the
// first step, which is From itself.
type PathStep struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Via   string `json:"via,omitempty"`
}

// FindPathOut is the output of find_path. Found is false, with an empty
// Path, when no route exists within Depth hops — not an error, the same
// way a zero-result search isn't one.
type FindPathOut struct {
	Found bool       `json:"found"`
	Path  []PathStep `json:"path"`
}

// FindPath finds the shortest chain of wikilinks connecting two notes —
// "how, if at all, are these related" for two specific notes, as opposed
// to get_graph's "what's in this note's neighborhood."
func (t *Tools) FindPath(_ context.Context, in FindPathIn) (FindPathOut, error) {
	fromRel, err := vault.SafeRequestPath(t.VaultRoot, in.From)
	if err != nil {
		return FindPathOut{}, fmt.Errorf("invalid from %q: %w", in.From, err)
	}
	toRel, err := vault.SafeRequestPath(t.VaultRoot, in.To)
	if err != nil {
		return FindPathOut{}, fmt.Errorf("invalid to %q: %w", in.To, err)
	}
	if _, ok := t.Index.Get(fromRel); !ok {
		return FindPathOut{}, fmt.Errorf("no such note: %s", fromRel)
	}
	if _, ok := t.Index.Get(toRel); !ok {
		return FindPathOut{}, fmt.Errorf("no such note: %s", toRel)
	}

	depth := 4
	if in.Depth != 0 {
		if in.Depth < 1 || in.Depth > 6 {
			return FindPathOut{}, fmt.Errorf("depth must be between 1 and 6")
		}
		depth = in.Depth
	}
	direction := in.Direction
	if direction == "" {
		direction = "both"
	}
	if direction != "out" && direction != "in" && direction != "both" {
		return FindPathOut{}, fmt.Errorf("direction must be 'out', 'in', or 'both'")
	}

	type pred struct{ prev, via string }
	preds := map[string]pred{fromRel: {}}
	frontier := []string{fromRel}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, p := range frontier {
			var edges []vault.Edge
			if direction == "out" || direction == "both" {
				edges = append(edges, t.Index.Outlinks(p)...)
			}
			if direction == "in" || direction == "both" {
				edges = append(edges, t.Index.Backlinks(p)...)
			}
			for _, e := range edges {
				other := e.To
				if other == p {
					other = e.From
				}
				if _, seen := preds[other]; seen {
					continue
				}
				preds[other] = pred{prev: p, via: e.Context}
				next = append(next, other)
			}
		}
		frontier = next
	}

	if _, ok := preds[toRel]; !ok {
		t.logResult("find_path", 0)
		return FindPathOut{Found: false, Path: []PathStep{}}, nil
	}

	var chain []string
	for cur := toRel; ; {
		chain = append(chain, cur)
		if cur == fromRel {
			break
		}
		cur = preds[cur].prev
	}
	slices.Reverse(chain)

	steps := make([]PathStep, len(chain))
	for i, p := range chain {
		n, _ := t.Index.Get(p)
		steps[i] = PathStep{Path: p, Title: n.Title}
		if i > 0 {
			steps[i].Via = preds[p].via
		}
	}

	t.logResult("find_path", len(steps))
	return FindPathOut{Found: true, Path: steps}, nil
}

// CreateNoteIn is the input to create_note.
type CreateNoteIn struct {
	Path        string         `json:"path" jsonschema:"required; vault-relative path for the new note"`
	Frontmatter map[string]any `json:"frontmatter,omitempty"`
	Body        string         `json:"body" jsonschema:"the markdown body"`
	// Reason, when given, becomes the body of the resulting git commit —
	// a one-line "why" helps whoever reads the vault's history later.
	Reason string `json:"reason,omitempty" jsonschema:"optional; becomes part of the commit message"`
}

// CreateNoteOut is the output of create_note.
type CreateNoteOut struct {
	Path string `json:"path"`
}

// CreateNote writes a brand-new note directly into the vault and commits
// it as its own, isolated git commit — the safety model for every write
// this server makes: a bad create is one `git revert` away from gone,
// never entangled with any other change, because there is never more
// than one change per commit. Fails if a note already exists at Path;
// use write_note to overwrite one deliberately. The new note is
// immediately visible to every read tool, no wait for the watcher.
func (t *Tools) CreateNote(_ context.Context, in CreateNoteIn) (CreateNoteOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return CreateNoteOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}
	if err := vault.CreateNote(t.VaultRoot, rel, in.Frontmatter, in.Body, in.Reason); err != nil {
		if errors.Is(err, vault.ErrNoteExists) {
			return CreateNoteOut{}, fmt.Errorf("a note already exists at %s; use write_note to overwrite it deliberately", rel)
		}
		return CreateNoteOut{}, err
	}
	t.Index.Refresh(rel) // make it visible now, not after the watcher's debounce
	t.logResult("create_note", len(in.Body))
	return CreateNoteOut{Path: rel}, nil
}

// WriteNoteIn is the input to write_note.
type WriteNoteIn struct {
	Path        string         `json:"path" jsonschema:"required; the note must already exist"`
	Frontmatter map[string]any `json:"frontmatter,omitempty"`
	Body        string         `json:"body" jsonschema:"the full new body, replacing the old one wholesale"`
	Reason      string         `json:"reason,omitempty" jsonschema:"optional; becomes part of the commit message"`
}

// WriteNoteOut is the output of write_note.
type WriteNoteOut struct {
	Path string `json:"path"`
	// Demoted is true when this write triggered R2: the note was thabit
	// and is now amil, because this edit made its prior review stale.
	Demoted bool `json:"demoted,omitempty"`
}

// WriteNote overwrites an existing note wholesale — the given
// frontmatter and body replace whatever was there, entirely — and
// commits the change as its own git commit. Fails if nothing exists yet
// at Path; use create_note for a new note. Fails if the note is asil —
// immutable to the model, no exception. If the note is currently thabit,
// this edit demotes it to amil in the same atomic write (R2); Demoted
// reports whether that happened, so the caller can mention it once.
func (t *Tools) WriteNote(_ context.Context, in WriteNoteIn) (WriteNoteOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return WriteNoteOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}
	demoted, err := vault.WriteNote(t.VaultRoot, rel, in.Frontmatter, in.Body, in.Reason)
	if err != nil {
		if errors.Is(err, vault.ErrNoteNotFound) {
			return WriteNoteOut{}, fmt.Errorf("no note exists yet at %s; use create_note first", rel)
		}
		if errors.Is(err, vault.ErrAsilLocked) {
			return WriteNoteOut{}, fmt.Errorf("%s is asil: immutable to the model, no exception", rel)
		}
		return WriteNoteOut{}, err
	}
	t.Index.Refresh(rel) // make the change visible now, not after the watcher's debounce
	t.logResult("write_note", len(in.Body))
	return WriteNoteOut{Path: rel, Demoted: demoted}, nil
}

// DeleteNoteIn is the input to delete_note.
type DeleteNoteIn struct {
	Path   string `json:"path" jsonschema:"required; the note must exist"`
	Reason string `json:"reason,omitempty" jsonschema:"optional; becomes part of the commit message"`
}

// DeleteNoteOut is the output of delete_note.
type DeleteNoteOut struct {
	Path string `json:"path"`
}

// DeleteNote removes a note and commits the removal as its own git
// commit. There is no confirmation step beyond the tool call itself —
// git is the undo mechanism (one `git revert` of exactly this commit),
// by design; see CreateNote's doc comment for the reasoning this shares.
// Only a dakhil note may be deleted — the permission matrix's one
// exception, since dakhil is the model's own working space.
func (t *Tools) DeleteNote(_ context.Context, in DeleteNoteIn) (DeleteNoteOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return DeleteNoteOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}
	if err := vault.DeleteNote(t.VaultRoot, rel, in.Reason); err != nil {
		if errors.Is(err, vault.ErrNoteNotFound) {
			return DeleteNoteOut{}, fmt.Errorf("no such note: %s", rel)
		}
		return DeleteNoteOut{}, err
	}
	t.Index.Refresh(rel) // drop it from the index now, not after the watcher's debounce
	t.logResult("delete_note", 1)
	return DeleteNoteOut{Path: rel}, nil
}

// TierGetIn is the input to tier_get.
type TierGetIn struct {
	Path string `json:"path" jsonschema:"required; the note to inspect"`
}

// TierProposalOut mirrors a note's outstanding proposal, if any.
type TierProposalOut struct {
	Tier   string    `json:"tier"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// TierGetOut is the output of tier_get.
type TierGetOut struct {
	Path     string           `json:"path"`
	Tier     string           `json:"tier"`
	Since    time.Time        `json:"since"`
	Proposed *TierProposalOut `json:"proposed,omitempty"`
}

// TierGet returns a note's current tier, when it last changed, and any
// outstanding proposal — read-only, always available to the model.
func (t *Tools) TierGet(_ context.Context, in TierGetIn) (TierGetOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return TierGetOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}
	n, ok := t.Index.Get(rel)
	if !ok {
		return TierGetOut{}, fmt.Errorf("no such note: %s", rel)
	}
	out := TierGetOut{Path: rel, Tier: string(n.Tier), Since: n.UpdatedAt}
	if p := vault.ProposalFrom(n.Frontmatter); p != nil {
		out.Proposed = &TierProposalOut{Tier: string(p.Tier), Reason: p.Reason, At: p.At}
	}
	t.logResult("tier_get", 1)
	return out, nil
}

// TierSetIn is the input to tier_set.
type TierSetIn struct {
	Path   string `json:"path" jsonschema:"required"`
	Tier   string `json:"tier" jsonschema:"required; must be strictly lower than the note's current tier"`
	Reason string `json:"reason" jsonschema:"required; written to the note's own tier_history, not just the commit"`
}

// TierSetOut is the output of tier_set.
type TierSetOut struct {
	Path string `json:"path"`
	Tier string `json:"tier"`
}

// TierSet lowers a note's tier, and only lowers it — R1: any call that
// would raise or hold the tier fails with an explicit error rather than
// silently doing nothing, because a silent no-op would teach the model
// the call worked. This is the model's only write path to the tier field
// at all, and it is one-directional by construction.
func (t *Tools) TierSet(_ context.Context, in TierSetIn) (TierSetOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return TierSetOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}
	target := vault.Tier(in.Tier)
	if !target.Valid() {
		return TierSetOut{}, fmt.Errorf("unknown tier %q; must be one of dakhil, amil, thabit, asil", in.Tier)
	}
	if err := vault.SetTier(t.VaultRoot, rel, target, in.Reason); err != nil {
		switch {
		case errors.Is(err, vault.ErrNoteNotFound):
			return TierSetOut{}, fmt.Errorf("no such note: %s", rel)
		case errors.Is(err, vault.ErrTierRaise):
			return TierSetOut{}, fmt.Errorf("R1: the model can never raise a tier; promotion is a human act, via Al-Mina")
		case errors.Is(err, vault.ErrAsilLocked):
			return TierSetOut{}, fmt.Errorf("%s is asil: immutable to the model, no exception", rel)
		}
		return TierSetOut{}, err
	}
	t.Index.Refresh(rel)
	t.logResult("tier_set", 1)
	return TierSetOut{Path: rel, Tier: in.Tier}, nil
}

// TierProposeIn is the input to tier_propose.
type TierProposeIn struct {
	Path   string `json:"path" jsonschema:"required"`
	Tier   string `json:"tier" jsonschema:"required; the tier being proposed"`
	Reason string `json:"reason" jsonschema:"required"`
}

// TierProposeOut is the output of tier_propose.
type TierProposeOut struct {
	Path string `json:"path"`
}

// TierPropose writes a proposal into the note's frontmatter for the user
// to act on in Al-Mina — it changes nothing else, and it is not itself a
// promotion. Rejects a proposal matching an unexpired denial for the
// same target tier.
func (t *Tools) TierPropose(_ context.Context, in TierProposeIn) (TierProposeOut, error) {
	rel, err := vault.SafeRequestPath(t.VaultRoot, in.Path)
	if err != nil {
		return TierProposeOut{}, fmt.Errorf("invalid path %q: %w", in.Path, err)
	}
	target := vault.Tier(in.Tier)
	if !target.Valid() {
		return TierProposeOut{}, fmt.Errorf("unknown tier %q; must be one of dakhil, amil, thabit, asil", in.Tier)
	}
	if err := vault.ProposeTier(t.VaultRoot, rel, target, in.Reason); err != nil {
		switch {
		case errors.Is(err, vault.ErrNoteNotFound):
			return TierProposeOut{}, fmt.Errorf("no such note: %s", rel)
		case errors.Is(err, vault.ErrAsilLocked):
			return TierProposeOut{}, fmt.Errorf("%s is already asil; there is no tier above it to propose", rel)
		case errors.Is(err, vault.ErrProposalDenied):
			return TierProposeOut{}, fmt.Errorf("a proposal to raise %s to %s was already denied, and the note hasn't changed since", rel, in.Tier)
		}
		return TierProposeOut{}, err
	}
	t.Index.Refresh(rel)
	t.logResult("tier_propose", 1)
	return TierProposeOut{Path: rel}, nil
}
