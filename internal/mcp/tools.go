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
//
// Index is a vault.Reader rather than a concrete *vault.Index so it can
// be either a plain vault-only index or a vault.Combined merging in the
// inbox. InboxRoot gates the write tools: empty means "no inbox
// configured," and create_note/write_note are not registered at all
// (see server.go) rather than registered and failing at call time.
type Tools struct {
	Index       vault.Reader
	Search      *search.Searcher
	InboxSearch *search.Searcher // nil disables inbox body search; see safeInboxPath
	VaultRoot   string
	InboxRoot   string // empty disables create_note/write_note
	// InboxIndex is the concrete inbox index CreateNote/WriteNote refresh
	// synchronously after a successful disk write, so a get_note right
	// after a create_note never races the watcher's debounce window. Nil
	// exactly when InboxRoot is empty.
	InboxIndex   *vault.Index
	MaxBodyBytes int64
	Log          *slog.Logger
}

// InboxPrefix is the reserved namespace inbox notes are addressed under
// in every tool argument and result — "inbox/foo.md" always means the
// inbox, never a literal vault folder named "inbox". cmd/nullmcp passes
// this exact value to vault.NewCombined so the two stay in lockstep.
const InboxPrefix = "inbox/"

// inboxLabel is appended to an inbox note's title in every tool result
// that carries one. A word in the text a model is already reading is
// harder to miss than a same-shaped extra JSON field — the Source field
// is still there too, for anything that filters on it.
const inboxLabel = " [inbox — draft, not yet reviewed or promoted]"

// displayTitle returns n.Title, suffixed with inboxLabel when n came
// from the inbox. Every tool output that shows a title runs it through
// this, so the "not yet settled" signal is impossible to miss whether a
// model is skimming a list or looking at one note.
func displayTitle(n *vault.Note) string {
	if n.Source == vault.SourceInbox {
		return n.Title + inboxLabel
	}
	return n.Title
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

// safeMergedPath validates a path exactly as any tool result returns
// it — vault-relative as-is, or InboxPrefix-prefixed — against whichever
// physical root actually owns it. This mirrors how vault.Combined itself
// routes a merged path, so safety is enforced before the path reaches
// the index at all, on the correct root either way.
func (t *Tools) safeMergedPath(merged string) (string, error) {
	if rel, ok := strings.CutPrefix(merged, InboxPrefix); ok {
		if t.InboxRoot == "" {
			return "", fmt.Errorf("inbox is not configured on this server")
		}
		safeRel, err := vault.SafeRequestPath(t.InboxRoot, rel)
		if err != nil {
			return "", err
		}
		return InboxPrefix + safeRel, nil
	}
	return vault.SafeRequestPath(t.VaultRoot, merged)
}

// safeInboxPath validates a path that must name something in the
// inbox — used only by create_note and write_note, the sole tools
// allowed to touch disk, and only ever under InboxRoot. Returns the
// physical inbox-relative path with InboxPrefix stripped.
func (t *Tools) safeInboxPath(merged string) (string, error) {
	rel, ok := strings.CutPrefix(merged, InboxPrefix)
	if !ok {
		return "", fmt.Errorf("path must start with %q — the inbox is the only writable location", InboxPrefix)
	}
	return vault.SafeRequestPath(t.InboxRoot, rel)
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
	ApproxTokens  int64  `json:"approx_tokens"`
	OutlinkCount  int    `json:"outlink_count"`
	BacklinkCount int    `json:"backlink_count"`
	Source        string `json:"source"` // "vault" or "inbox" — see Title
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
		Title:         displayTitle(n),
		Tags:          tags,
		Frontmatter:   n.Frontmatter,
		UpdatedAt:     n.UpdatedAt,
		SizeBytes:     n.SizeBytes,
		ApproxTokens:  n.SizeBytes / 4,
		OutlinkCount:  len(n.Outlinks),
		BacklinkCount: len(t.Index.Backlinks(n.Path)),
		Source:        n.Source,
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
	Source      string          `json:"source"` // "vault" or "inbox" — see Title
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
	rel, err := t.safeMergedPath(in.Path)
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
		Title:       displayTitle(n),
		Source:      n.Source,
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
	Source  string         `json:"source"` // "vault" or "inbox" — see Title
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
		type rawHit struct {
			path    string
			matches []search.Match
		}
		var raw []rawHit

		results, err := t.Search.Search(ctx, in.Query)
		if err != nil {
			return SearchNotesOut{}, fmt.Errorf("search backend: %w", err)
		}
		for _, res := range results {
			raw = append(raw, rawHit{path: res.Path, matches: res.Matches})
		}
		// Ripgrep only sees NULL_VAULT_PATH; the inbox is a second
		// process over a second root, merged here by prefixing its
		// paths the same way Combined does for every other read.
		if t.InboxSearch != nil {
			inboxResults, err := t.InboxSearch.Search(ctx, in.Query)
			if err != nil {
				return SearchNotesOut{}, fmt.Errorf("inbox search backend: %w", err)
			}
			for _, res := range inboxResults {
				raw = append(raw, rawHit{path: InboxPrefix + res.Path, matches: res.Matches})
			}
		}

		maxCount := 0
		for _, rh := range raw {
			if len(rh.matches) > maxCount {
				maxCount = len(rh.matches)
			}
		}
		for _, rh := range raw {
			if !visible(rh.path) {
				continue
			}
			n, _ := t.Index.Get(rh.path)
			hits[rh.path] = &SearchHit{
				Path: rh.path, Title: displayTitle(n), Source: n.Source,
				Score: float64(len(rh.matches)) / float64(maxCount), Matches: rh.matches,
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
	Source   string `json:"source"` // "vault" or "inbox" — see Title
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
	rel, err := t.safeMergedPath(in.Path)
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
		title := n.Title
		if n.Source == vault.SourceInbox {
			title += inboxLabel
		}
		out.Nodes = append(out.Nodes, GraphNodeOut{Path: n.Path, Title: title, Distance: n.Distance, Source: n.Source})
	}
	for _, e := range g.Edges {
		out.Edges = append(out.Edges, GraphEdgeOut{From: e.From, To: e.To, Context: e.Context})
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
	Source     string   `json:"source"` // "vault" or "inbox" — see Title
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
	rel, err := t.safeMergedPath(in.Path)
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
			Path: o.Path, Title: displayTitle(o), Source: o.Source,
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
	Source  string `json:"source"` // "vault" or "inbox" — see Title
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
	rel, err := t.safeMergedPath(in.Path)
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
			ln.Title, ln.Source = displayTitle(n), n.Source
		}
		out.Outlinks = append(out.Outlinks, ln)
	}
	for _, e := range t.Index.Backlinks(rel) {
		ln := LinkedNote{Path: e.From, Context: e.Context}
		if n, ok := t.Index.Get(e.From); ok {
			ln.Title, ln.Source = displayTitle(n), n.Source
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
	Path   string `json:"path"`
	Title  string `json:"title"`
	Source string `json:"source"` // "vault" or "inbox" — see Title
	Via    string `json:"via,omitempty"`
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
// to get_graph's "what's in this note's neighborhood." Like get_graph,
// it does not cross the vault/inbox boundary (see package doc on
// vault.Combined); a path through a promoted note works once promoted.
func (t *Tools) FindPath(_ context.Context, in FindPathIn) (FindPathOut, error) {
	fromRel, err := t.safeMergedPath(in.From)
	if err != nil {
		return FindPathOut{}, fmt.Errorf("invalid from %q: %w", in.From, err)
	}
	toRel, err := t.safeMergedPath(in.To)
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
		steps[i] = PathStep{Path: p, Title: displayTitle(n), Source: n.Source}
		if i > 0 {
			steps[i].Via = preds[p].via
		}
	}

	t.logResult("find_path", len(steps))
	return FindPathOut{Found: true, Path: steps}, nil
}

// CreateNoteIn is the input to create_note.
type CreateNoteIn struct {
	Path        string         `json:"path" jsonschema:"required; must start with 'inbox/' — the only writable location"`
	Frontmatter map[string]any `json:"frontmatter,omitempty"`
	Body        string         `json:"body" jsonschema:"the markdown body"`
}

// CreateNoteOut is the output of create_note.
type CreateNoteOut struct {
	Path string `json:"path"`
}

// CreateNote writes a brand-new note into the inbox — the only location
// any tool in this server can write to; the vault itself is never
// touched. Fails if a note already exists at Path; use write_note to
// overwrite one deliberately. The new note is immediately visible to
// every read tool, labeled as inbox, exactly like any other note except
// for that label.
func (t *Tools) CreateNote(_ context.Context, in CreateNoteIn) (CreateNoteOut, error) {
	if t.InboxRoot == "" {
		return CreateNoteOut{}, fmt.Errorf("inbox is not configured on this server")
	}
	physRel, err := t.safeInboxPath(in.Path)
	if err != nil {
		return CreateNoteOut{}, err
	}
	if err := vault.CreateNote(t.InboxRoot, physRel, in.Frontmatter, in.Body); err != nil {
		if errors.Is(err, vault.ErrNoteExists) {
			return CreateNoteOut{}, fmt.Errorf(
				"a note already exists at %s%s; use write_note to overwrite it deliberately", InboxPrefix, physRel)
		}
		return CreateNoteOut{}, err
	}
	t.InboxIndex.Refresh(physRel) // make it visible now, not after the watcher's debounce
	merged := InboxPrefix + physRel
	t.logResult("create_note", len(in.Body))
	return CreateNoteOut{Path: merged}, nil
}

// WriteNoteIn is the input to write_note.
type WriteNoteIn struct {
	Path        string         `json:"path" jsonschema:"required; must start with 'inbox/'; the note must already exist"`
	Frontmatter map[string]any `json:"frontmatter,omitempty"`
	Body        string         `json:"body" jsonschema:"the full new body, replacing the old one wholesale"`
}

// WriteNoteOut is the output of write_note.
type WriteNoteOut struct {
	Path string `json:"path"`
}

// WriteNote overwrites an existing inbox note wholesale — the given
// frontmatter and body replace whatever was there, entirely. Fails if
// nothing exists yet at Path; use create_note for a new note. Like
// CreateNote, this only ever touches the inbox, never the vault.
func (t *Tools) WriteNote(_ context.Context, in WriteNoteIn) (WriteNoteOut, error) {
	if t.InboxRoot == "" {
		return WriteNoteOut{}, fmt.Errorf("inbox is not configured on this server")
	}
	physRel, err := t.safeInboxPath(in.Path)
	if err != nil {
		return WriteNoteOut{}, err
	}
	if err := vault.WriteNote(t.InboxRoot, physRel, in.Frontmatter, in.Body); err != nil {
		if errors.Is(err, vault.ErrNoteNotFound) {
			return WriteNoteOut{}, fmt.Errorf(
				"no note exists yet at %s%s; use create_note first", InboxPrefix, physRel)
		}
		return WriteNoteOut{}, err
	}
	t.InboxIndex.Refresh(physRel) // make the change visible now, not after the watcher's debounce
	merged := InboxPrefix + physRel
	t.logResult("write_note", len(in.Body))
	return WriteNoteOut{Path: merged}, nil
}
