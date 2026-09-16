package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"null-service/internal/search"
	"null-service/internal/vault"
)

const fixtureVault = "../../testdata/vault"

func testTools(t *testing.T) *Tools {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	ix := vault.NewIndex(fixtureVault, log)
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	se, err := search.New(fixtureVault)
	if err != nil {
		t.Fatalf("search.New: %v (is ripgrep installed?)", err)
	}
	return &Tools{Index: ix, VaultIndex: ix, Search: se, VaultRoot: fixtureVault, MaxBodyBytes: 200_000, Log: log}
}

func TestListNotesMetadataOnly(t *testing.T) {
	out, err := testTools(t).ListNotes(context.Background(), ListNotesIn{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Notes) != 6 {
		t.Fatalf("got %d notes, want 6", len(out.Notes))
	}
	// Belt and braces: even though NoteSummary has no Body field, prove
	// the wire format never grows one silently.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"body"`) {
		t.Fatal("list_notes leaked a body field")
	}
	for _, n := range out.Notes {
		if n.Path == "engineering/basim/soul.md" {
			if n.OutlinkCount != 3 || n.BacklinkCount != 3 || n.ApproxTokens != n.SizeBytes/4 {
				t.Fatalf("soul summary wrong: %+v", n)
			}
			return
		}
	}
	t.Fatal("soul.md not listed")
}

func TestListNotesFiltersAndCursor(t *testing.T) {
	tl := testTools(t)

	out, err := tl.ListNotes(context.Background(), ListNotesIn{Folder: "engineering/", Tags: []string{"spec"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Notes) != 1 || out.Notes[0].Path != "engineering/basim/soul.md" {
		t.Fatalf("filtered = %+v", out.Notes)
	}

	// walk the whole vault via cursor, sorted by path, 2 at a time
	var all []string
	cursor := ""
	for range 10 {
		page, err := tl.ListNotes(context.Background(), ListNotesIn{Sort: "path", Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Notes) > 2 {
			t.Fatalf("page larger than limit: %d", len(page.Notes))
		}
		for _, n := range page.Notes {
			all = append(all, n.Path)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(all) != 6 {
		t.Fatalf("paginated walk returned %d notes: %v", len(all), all)
	}

	if _, err := tl.ListNotes(context.Background(), ListNotesIn{Cursor: "nope.md"}); err == nil {
		t.Fatal("expected error for unknown cursor")
	}
}

func TestGetNote(t *testing.T) {
	tl := testTools(t)

	out, err := tl.GetNote(context.Background(), GetNoteIn{
		Path: "engineering/basim/soul.md", Include: []string{"body", "outlinks", "backlinks"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Body, "# soul") {
		t.Fatalf("body missing content: %q", out.Body)
	}
	if len(out.Outlinks) != 2 || len(out.Backlinks) != 3 {
		t.Fatalf("outlinks=%v backlinks=%v", out.Outlinks, out.Backlinks)
	}

	sec, err := tl.GetNote(context.Background(), GetNoteIn{Path: "engineering/basim/soul.md", Section: "Failure modes"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sec.Body, "## Failure modes") || strings.Contains(sec.Body, "Who I am") {
		t.Fatalf("section wrong: %q", sec.Body)
	}

	if _, err := tl.GetNote(context.Background(), GetNoteIn{Path: "engineering/basim/soul.md", Section: "Nope"}); err == nil {
		t.Fatal("expected error for unknown section")
	}
	if _, err := tl.GetNote(context.Background(), GetNoteIn{Path: "../../etc/passwd"}); err == nil {
		t.Fatal("expected error for unsafe path")
	}
	if _, err := tl.GetNote(context.Background(), GetNoteIn{Path: ".hidden/secret.md"}); err == nil {
		t.Fatal("expected error for hidden note")
	}
	if _, err := tl.GetNote(context.Background(), GetNoteIn{Path: "plain.md", Include: []string{"nope"}}); err == nil {
		t.Fatal("expected error for unknown include value")
	}
}

func TestGetNoteBodyCap(t *testing.T) {
	tl := testTools(t)
	tl.MaxBodyBytes = 10
	_, err := tl.GetNote(context.Background(), GetNoteIn{Path: "plain.md"})
	if err == nil || !strings.Contains(err.Error(), "section") {
		t.Fatalf("expected a cap error pointing at section, got %v", err)
	}
}

func TestSearchNotesSnippetsOnly(t *testing.T) {
	tl := testTools(t)

	out, err := tl.SearchNotes(context.Background(), SearchNotesIn{Query: "sirr"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Path != "philosophy/barzakh.md" {
		t.Fatalf("results = %+v", out.Results)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"body"`) {
		t.Fatal("search_notes leaked a body field")
	}

	if _, err := tl.SearchNotes(context.Background(), SearchNotesIn{}); err == nil {
		t.Fatal("expected error for empty query")
	}

	titleOut, err := tl.SearchNotes(context.Background(), SearchNotesIn{Query: "soul", In: "title"})
	if err != nil {
		t.Fatal(err)
	}
	if len(titleOut.Results) != 1 || titleOut.Results[0].Path != "engineering/basim/soul.md" || titleOut.Results[0].Score != 1.0 {
		t.Fatalf("title search = %+v", titleOut.Results)
	}
}

func TestGetGraph(t *testing.T) {
	tl := testTools(t)

	out, err := tl.GetGraph(context.Background(), GetGraphIn{
		Path: "engineering/basim/character.md", Depth: 2, Direction: "out",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 2 || len(out.Edges) != 4 {
		t.Fatalf("nodes=%d edges=%d", len(out.Nodes), len(out.Edges))
	}
	for _, e := range out.Edges {
		if e.Context == "" {
			t.Fatalf("edge %s->%s missing context", e.From, e.To)
		}
	}

	if _, err := tl.GetGraph(context.Background(), GetGraphIn{Path: "plain.md", Depth: 4}); err == nil {
		t.Fatal("expected error for depth > 3")
	}
	if _, err := tl.GetGraph(context.Background(), GetGraphIn{Path: "nope.md"}); err == nil {
		t.Fatal("expected error for unknown note")
	}
}

func TestFindRelatives(t *testing.T) {
	tl := testTools(t)

	// character.md shares folder and the "basim" tag with soul.md
	out, err := tl.FindRelatives(context.Background(), FindRelativesIn{Path: "engineering/basim/character.md"})
	if err != nil {
		t.Fatal(err)
	}
	var found *Relative
	for i := range out.Relatives {
		if out.Relatives[i].Path == "engineering/basim/soul.md" {
			found = &out.Relatives[i]
		}
	}
	if found == nil || !found.SameFolder || !slices.Contains(found.SharedTags, "basim") {
		t.Fatalf("expected soul.md as a same-folder, shared-tag relative: %+v", out.Relatives)
	}

	// by=tags excludes same-folder-only notes with no shared tag
	byTags, err := tl.FindRelatives(context.Background(), FindRelativesIn{Path: "index.md", By: "tags"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range byTags.Relatives {
		if len(r.SharedTags) == 0 {
			t.Fatalf("by=tags returned a note with no shared tag: %+v", r)
		}
	}

	if _, err := tl.FindRelatives(context.Background(), FindRelativesIn{Path: "nope.md"}); err == nil {
		t.Fatal("expected error for unknown note")
	}
	if _, err := tl.FindRelatives(context.Background(), FindRelativesIn{Path: "plain.md", By: "sideways"}); err == nil {
		t.Fatal("expected error for bad by value")
	}
}

func TestGetLinks(t *testing.T) {
	tl := testTools(t)

	out, err := tl.GetLinks(context.Background(), GetLinksIn{Path: "engineering/basim/soul.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Outlinks) != 3 { // character (x2 lines) + index
		t.Fatalf("outlinks = %+v", out.Outlinks)
	}
	if len(out.Backlinks) != 3 { // character, index, barzakh
		t.Fatalf("backlinks = %+v", out.Backlinks)
	}
	for _, l := range out.Outlinks {
		if l.Context == "" || l.Title == "" {
			t.Fatalf("outlink missing context/title: %+v", l)
		}
	}

	if _, err := tl.GetLinks(context.Background(), GetLinksIn{Path: "nope.md"}); err == nil {
		t.Fatal("expected error for unknown note")
	}
}

func TestFindPath(t *testing.T) {
	tl := testTools(t)

	// character.md -> soul.md is a direct out-link
	out, err := tl.FindPath(context.Background(), FindPathIn{
		From: "engineering/basim/character.md", To: "engineering/basim/soul.md", Direction: "out",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || len(out.Path) != 2 {
		t.Fatalf("path = %+v", out.Path)
	}
	if out.Path[0].Path != "engineering/basim/character.md" || out.Path[1].Path != "engineering/basim/soul.md" {
		t.Fatalf("path order wrong: %+v", out.Path)
	}
	if out.Path[0].Via != "" || out.Path[1].Via == "" {
		t.Fatalf("via should be empty on the first step only: %+v", out.Path)
	}

	// same note, trivial path of length 1
	trivial, err := tl.FindPath(context.Background(), FindPathIn{From: "plain.md", To: "plain.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !trivial.Found || len(trivial.Path) != 1 {
		t.Fatalf("trivial path = %+v", trivial.Path)
	}

	// no route within the depth searched is Found:false, not an error
	unreachable, err := tl.FindPath(context.Background(), FindPathIn{
		From: "malformed.md", To: "engineering/basim/soul.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	if unreachable.Found {
		t.Fatalf("expected no path, got %+v", unreachable.Path)
	}

	if _, err := tl.FindPath(context.Background(), FindPathIn{From: "nope.md", To: "plain.md"}); err == nil {
		t.Fatal("expected error for unknown from")
	}
	if _, err := tl.FindPath(context.Background(), FindPathIn{From: "plain.md", To: "plain.md", Depth: 7}); err == nil {
		t.Fatal("expected error for depth > 6")
	}
}

// buildToolsWithInbox is testTools plus an inbox index sharing the same
// physical directory an actual inbox would use, for testing the
// "_vault_only" tools against a real merged Index the way nullmcp builds
// one — not just the bare vault index testTools alone would give.
func buildToolsWithInbox(t *testing.T, inboxFiles map[string]string) *Tools {
	t.Helper()
	log := slog.New(slog.DiscardHandler)

	primary := vault.NewIndex(fixtureVault, log)
	if err := primary.Build(); err != nil {
		t.Fatal(err)
	}
	se, err := search.New(fixtureVault)
	if err != nil {
		t.Fatalf("search.New: %v (is ripgrep installed?)", err)
	}

	inboxRoot := t.TempDir()
	for rel, content := range inboxFiles {
		if err := vault.CreateNote(inboxRoot, rel, nil, content); err != nil {
			t.Fatal(err)
		}
	}
	inboxIx := vault.NewIndex(inboxRoot, log)
	inboxIx.Source = vault.SourceInbox
	if err := inboxIx.Build(); err != nil {
		t.Fatal(err)
	}

	return &Tools{
		Index: vault.NewCombined(primary, inboxIx, InboxPrefix), VaultIndex: primary,
		Search: se, VaultRoot: fixtureVault, InboxRoot: inboxRoot, InboxIndex: inboxIx,
		MaxBodyBytes: 200_000, Log: log,
	}
}

func TestGetGraphVaultOnlyExcludesInbox(t *testing.T) {
	tl := buildToolsWithInbox(t, map[string]string{
		"draft.md": "# draft\n\nlinks to [[soul]]\n",
	})

	// the merged tool can root a graph at an inbox note
	merged, err := tl.GetGraph(context.Background(), GetGraphIn{Path: "inbox/draft.md"})
	if err != nil {
		t.Fatal(err)
	}
	if merged.Root != "inbox/draft.md" {
		t.Fatalf("merged root = %q", merged.Root)
	}

	// the vault-only tool refuses the same root outright
	if _, err := tl.GetGraphVaultOnly(context.Background(), GetGraphIn{Path: "inbox/draft.md"}); err == nil {
		t.Fatal("expected error rooting a vault-only graph at an inbox note")
	}

	// and never surfaces an inbox node when rooted in the vault, even
	// though that already held true before this tool existed (no
	// cross-boundary edges yet) — asserted so a future change to that
	// limitation can't silently leak inbox content in here too
	vaultOnly, err := tl.GetGraphVaultOnly(context.Background(), GetGraphIn{
		Path: "engineering/basim/soul.md", Depth: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range vaultOnly.Nodes {
		if n.Source == vault.SourceInbox {
			t.Fatalf("vault-only graph leaked an inbox node: %+v", n)
		}
	}
}

func TestGetGraphVaultOnlyWorksWithoutInboxConfigured(t *testing.T) {
	tl := testTools(t) // no inbox at all
	out, err := tl.GetGraphVaultOnly(context.Background(), GetGraphIn{Path: "engineering/basim/soul.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) == 0 {
		t.Fatal("expected a normal graph with no inbox configured")
	}
}

func TestFindPathVaultOnlyExcludesInbox(t *testing.T) {
	tl := buildToolsWithInbox(t, map[string]string{
		"draft.md": "# draft\n\nlinks to [[soul]]\n",
	})

	// the merged tool accepts an inbox endpoint (even though, per the
	// documented limitation, it won't find a cross-boundary route)
	if _, err := tl.FindPath(context.Background(), FindPathIn{
		From: "inbox/draft.md", To: "inbox/draft.md",
	}); err != nil {
		t.Fatalf("merged find_path should accept an inbox endpoint: %v", err)
	}

	// the vault-only tool refuses an inbox endpoint outright
	if _, err := tl.FindPathVaultOnly(context.Background(), FindPathIn{
		From: "inbox/draft.md", To: "engineering/basim/soul.md",
	}); err == nil {
		t.Fatal("expected error for an inbox endpoint in find_path_vault_only")
	}

	// ordinary vault-to-vault pathfinding still works
	out, err := tl.FindPathVaultOnly(context.Background(), FindPathIn{
		From: "engineering/basim/character.md", To: "engineering/basim/soul.md", Direction: "out",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || len(out.Path) != 2 {
		t.Fatalf("path = %+v", out.Path)
	}
}
