package mcp

import (
	"context"
	"log/slog"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"null-service/internal/search"
	"null-service/internal/vault"
)

// serverInfo names this server to clients during MCP initialization.
var serverInfo = &sdkmcp.Implementation{Name: "null", Version: "0.1.0"}

// NewServer builds an MCP server exposing the vault as four tools and
// registers them. It assumes ix and se are already built and verified —
// the same objects nullapi would use — and shares them directly, in
// process: this is a second presentation of the read API, not a second
// implementation of it. The returned server writes nothing to the vault.
func NewServer(ix *vault.Index, se *search.Searcher, vaultRoot string, maxBodyBytes int64, log *slog.Logger) *sdkmcp.Server {
	t := &Tools{
		Index:        ix,
		Search:       se,
		VaultRoot:    vaultRoot,
		MaxBodyBytes: maxBodyBytes,
		Log:          log,
	}

	s := sdkmcp.NewServer(serverInfo, nil)

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "list_notes",
		Description: "List notes in the vault, filterable by folder/tag/recency. " +
			"Returns metadata only — title, tags, frontmatter, size, and " +
			"approx_tokens — never a body. Call this or search_notes before " +
			"get_note to find the right path first; fetching every note's body " +
			"just to browse would blow the context window.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in ListNotesIn) (*sdkmcp.CallToolResult, ListNotesOut, error) {
		out, err := t.ListNotes(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "get_note",
		Description: "Fetch one note by its exact vault path. The only tool that " +
			"returns a body, and even then it is capped — pass section (a " +
			"heading's exact text) to pull just that heading instead of the " +
			"whole note. Check approx_tokens from list_notes/search_notes first " +
			"and prefer section on anything large.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in GetNoteIn) (*sdkmcp.CallToolResult, GetNoteOut, error) {
		out, err := t.GetNote(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "search_notes",
		Description: "Full-text search over the vault. Case- and diacritic-" +
			"insensitive (e.g. 'sirr' matches 'Şirr'). Returns line-numbered " +
			"snippets, never bodies — use a hit's path with get_note to read " +
			"further, ideally with section set to the heading nearest the match.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in SearchNotesIn) (*sdkmcp.CallToolResult, SearchNotesOut, error) {
		out, err := t.SearchNotes(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "get_graph",
		Description: "BFS neighborhood of one note's wikilinks, direction " +
			"out/in/both, up to depth 3. Each edge carries the exact line its " +
			"link was written on — the stated reason two notes are connected, " +
			"not just that they are. Cheaper than fetching notes to rediscover " +
			"their relationships.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in GetGraphIn) (*sdkmcp.CallToolResult, GetGraphOut, error) {
		out, err := t.GetGraph(ctx, in)
		return nil, out, err
	})

	return s
}
