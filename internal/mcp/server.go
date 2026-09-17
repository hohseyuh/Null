package mcp

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverInfo names this server to clients during MCP initialization.
var serverInfo = &sdkmcp.Implementation{Name: "null", Version: "0.1.0"}

// NewServer builds an MCP server exposing t's tools and registers them.
// It assumes t's dependencies are already built and verified — the same
// objects nullapi would use — and shares them directly, in process: this
// is a second presentation of the read API, not a second implementation
// of it.
//
// All thirteen tools are always registered — writes are unconditional
// (see CLAUDE.md non-negotiable #2 and vault/write.go's package doc):
// every create_note, write_note, and delete_note is its own isolated git
// commit. There is no push or commit tool exposed to the model anywhere
// in this server — see spec/tiers.md's "One door" — and no MCP path ever
// raises a note's tier; tier_set only lowers, tier_propose only asks.
func NewServer(t *Tools) *sdkmcp.Server {
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
		Description: "Fetch one note by its exact vault-relative path. The only " +
			"tool that returns a body, and even then it is capped — pass section " +
			"(a heading's exact text) to pull just that heading instead of the " +
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

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "find_relatives",
		Description: "Find notes related to one note by folder and/or shared tags " +
			"— organizational/topical proximity, not the link graph (use get_graph " +
			"or get_links for that). Two notes can be relatives with no link " +
			"between them at all.",
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
			"more than one hop.",
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
			"an error.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in FindPathIn) (*sdkmcp.CallToolResult, FindPathOut, error) {
		out, err := t.FindPath(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "create_note",
		Description: "Create a new note directly in the vault. Fails if a note " +
			"already exists at that path; use write_note to overwrite one on " +
			"purpose. Commits immediately as its own isolated git commit — " +
			"'Add <path>', plus reason if given — never bundled with any other " +
			"change. The note is immediately visible to every read tool here. " +
			"It starts at tier dakhil — the model's own working space — " +
			"regardless of any tier field passed in frontmatter, which is " +
			"ignored: tier is server-owned. This server never pushes to a " +
			"remote itself; that stays a human's `git push`.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in CreateNoteIn) (*sdkmcp.CallToolResult, CreateNoteOut, error) {
		out, err := t.CreateNote(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "write_note",
		Description: "Overwrite an existing note wholesale — the given body and " +
			"frontmatter replace what was there entirely, not a partial edit. " +
			"Fails if nothing exists yet at that path; use create_note for a new " +
			"note. Fails if the note is asil — immutable, no exception. If the " +
			"note is thabit, this edit demotes it to amil in the same atomic " +
			"write (R2: your prior review is now stale) — the response's " +
			"demoted field reports this; mention it once, plainly, if it fires. " +
			"Commits as 'Update <path>', plus reason if given — but if the very " +
			"last thing committed was itself a write_note update to this same " +
			"note, this amends that commit instead of stacking a new one, so " +
			"editing the same note repeatedly in a row makes one commit, not " +
			"one per call (only the latest reason is kept). Any other commit in " +
			"between — a different note, a create, a delete — breaks that chain.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in WriteNoteIn) (*sdkmcp.CallToolResult, WriteNoteOut, error) {
		out, err := t.WriteNote(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "delete_note",
		Description: "Delete a note from the vault. Only permitted on a dakhil " +
			"note — the model's own working space; amil, thabit, and asil notes " +
			"cannot be deleted this way. No confirmation step beyond this call " +
			"— git is the undo mechanism, deliberately: the deletion is its own " +
			"isolated commit ('Delete <path>', plus reason if given), so undoing " +
			"it is a single, clean git revert of exactly that commit, never " +
			"entangled with any other change.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in DeleteNoteIn) (*sdkmcp.CallToolResult, DeleteNoteOut, error) {
		out, err := t.DeleteNote(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "tier_get",
		Description: "Get one note's current tier (dakhil/amil/thabit/asil), " +
			"when it last changed, and any outstanding proposal. Read-only, " +
			"always available.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in TierGetIn) (*sdkmcp.CallToolResult, TierGetOut, error) {
		out, err := t.TierGet(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "tier_set",
		Description: "Lower a note's tier — and only lower it. R1: the model " +
			"can never raise a tier, not by this tool, not by any tool, not as " +
			"a side effect; a call that would raise or hold the tier fails with " +
			"an explicit error rather than silently doing nothing. Promotion is " +
			"a human act, via Al-Mina. reason is required and is written into " +
			"the note's own tier_history in frontmatter, not just the commit.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in TierSetIn) (*sdkmcp.CallToolResult, TierSetOut, error) {
		out, err := t.TierSet(ctx, in)
		return nil, out, err
	})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name: "tier_propose",
		Description: "Propose raising a note's tier, for the user to act on in " +
			"Al-Mina. Writes proposed_tier/proposed_reason only — it does not " +
			"change the note's actual tier or its body, and it is not itself a " +
			"promotion. Refuses a proposal matching a denial the note already " +
			"carries for the same target tier, unless the note has been edited " +
			"since the denial.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, in TierProposeIn) (*sdkmcp.CallToolResult, TierProposeOut, error) {
		out, err := t.TierPropose(ctx, in)
		return nil, out, err
	})

	return s
}
