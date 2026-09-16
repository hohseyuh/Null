package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
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
	return &Tools{Index: ix, Search: se, VaultRoot: fixtureVault, MaxBodyBytes: 200_000, Log: log}
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
