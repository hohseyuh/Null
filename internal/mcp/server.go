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

// inboxNote is appended to every read tool's description so a model
// never has to discover the inbox/labeling convention by trial and
// error — it's told up front, the same information the labeled results
// themselves carry.
const inboxNote = " Results may include inbox notes — unreviewed drafts, " +
	"titled with an '[inbox — draft, not yet reviewed or promoted]' suffix " +
	"and source:\"inbox\" — alongside settled vault notes; treat those as " +
	"provisional, not established fact."

// NewServer builds an MCP server exposing the vault as MCP tools and
// registers them. It assumes ix and se are already built and verified —
// the same objects nullapi would use — and shares them directly, in
// process: this is a second presentation of the read API, not a second
// implementation of it.
//
// create_note and write_note are registered only when inboxRoot is
// non-empty — an unconfigured inbox means no write tools exist at all,
// not write tools that exist and fail at call time. Nothing this server
// does ever opens a file under vaultRoot for anything but reading;
// inboxSearch and inboxIndex may be nil (together, when inboxRoot is
// empty) — inboxSearch's absence disables inbox coverage in
// search_notes' body mode, inboxIndex is what CreateNote/WriteNote
// refresh synchronously after a write (see Tools.InboxIndex).
func NewServer(ix vault.Reader, se, inboxSearch *search.Searcher, inboxIndex *vault.Index, vaultRoot, inboxRoot string, maxBodyBytes int64, log *slog.Logger) *sdkmcp.Server {
	t := &Tools{
		Index:        ix,
		Search:       se,
		InboxSearch:  inboxSearch,
		VaultRoot:    vaultRoot,
		InboxRoot:    inboxRoot,
		InboxIndex:   inboxIndex,
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
			"just to browse would blow the context window." + inboxNote,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in ListNotesIn) (*sdkmcp.CallToolResult, ListNotesOut, error) {
		out, err := t.ListNotes(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "get_note",
		Description: "Fetch one note by its exact path (as returned by any other " +
			"tool — vault paths as-is, inbox paths prefixed 'inbox/'). The only " +
			"tool that returns a body, and even then it is capped — pass section " +
			"(a heading's exact text) to pull just that heading instead of the " +
			"whole note. Check approx_tokens from list_notes/search_notes first " +
			"and prefer section on anything large." + inboxNote,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in GetNoteIn) (*sdkmcp.CallToolResult, GetNoteOut, error) {
		out, err := t.GetNote(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "search_notes",
		Description: "Full-text search over the vault. Case- and diacritic-" +
			"insensitive (e.g. 'sirr' matches 'Şirr'). Returns line-numbered " +
			"snippets, never bodies — use a hit's path with get_note to read " +
			"further, ideally with section set to the heading nearest the match." + inboxNote,
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
			"their relationships. Does not cross the vault/inbox boundary: a " +
			"draft's link to a settled note (or back) only becomes a graph edge " +
			"once the draft is promoted." + inboxNote,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in GetGraphIn) (*sdkmcp.CallToolResult, GetGraphOut, error) {
		out, err := t.GetGraph(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "find_relatives",
		Description: "Find notes related to one note by folder and/or shared tags " +
			"— organizational/topical proximity, not the link graph (use get_graph " +
			"or get_links for that). Two notes can be relatives with no link " +
			"between them at all." + inboxNote,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in FindRelativesIn) (*sdkmcp.CallToolResult, FindRelativesOut, error) {
		out, err := t.FindRelatives(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "get_links",
		Description: "One note's direct outlinks and backlinks, each with the " +
			"context line the wikilink was written on, as two separate lists. " +
			"Equivalent to get_graph at depth 1 direction both, split by " +
			"direction — cheaper to read when you want in vs. out and don't need " +
			"more than one hop." + inboxNote,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in GetLinksIn) (*sdkmcp.CallToolResult, GetLinksOut, error) {
		out, err := t.GetLinks(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "find_path",
		Description: "Shortest chain of wikilinks connecting two specific notes, " +
			"up to 6 hops — 'how are these two related, if at all' rather than " +
			"get_graph's 'what surrounds this one note.' found:false with an " +
			"empty path means no route was found within the depth searched, not " +
			"an error. Does not cross the vault/inbox boundary — see get_graph." + inboxNote,
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in FindPathIn) (*sdkmcp.CallToolResult, FindPathOut, error) {
		out, err := t.FindPath(ctx, in)
		return nil, out, err
	})

	if inboxRoot != "" {
		sdkmcp.AddTool(s, &sdkmcp.Tool{
			Name: "create_note",
			Description: "Create a new note. Path must start with 'inbox/' — the " +
				"vault itself is never writable by this server; the inbox is a " +
				"separate, physically distinct location a human reviews before " +
				"anything in it becomes part of the real vault (via their own git " +
				"commit — this tool never does that). Fails if a note already " +
				"exists at that path; use write_note to overwrite one on purpose. " +
				"The new note is immediately visible to every read tool here, " +
				"labeled as inbox.",
		}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in CreateNoteIn) (*sdkmcp.CallToolResult, CreateNoteOut, error) {
			out, err := t.CreateNote(ctx, in)
			return nil, out, err
		})

		sdkmcp.AddTool(s, &sdkmcp.Tool{
			Name: "write_note",
			Description: "Overwrite an existing inbox note wholesale — the given " +
				"body and frontmatter replace what was there entirely, not a " +
				"partial edit. Path must start with 'inbox/'. Fails if nothing " +
				"exists yet at that path; use create_note for a new note. Like " +
				"create_note, this can only ever touch the inbox, never the vault.",
		}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in WriteNoteIn) (*sdkmcp.CallToolResult, WriteNoteOut, error) {
			out, err := t.WriteNote(ctx, in)
			return nil, out, err
		})
	}

	return s
}
