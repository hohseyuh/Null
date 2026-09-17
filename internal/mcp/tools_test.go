package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"null-service/internal/search"
	"null-service/internal/vault"
)

const fixtureVault = "../../testdata/vault"

// testTools builds Tools directly over the committed fixture vault —
// read-only use. Never call a write tool against this: the fixture is
// this repo's own tracked test data, and testTools's vault isn't even a
// git repo (CreateNote/WriteNote/DeleteNote would fail EnsureGitRepo's
// precondition, by design — see gitVaultTools for a writable fixture).
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

// gitVaultTools copies the fixture vault into a fresh git repo (an
// initial commit seeding its current content) and builds Tools over
// that copy — for tests that call create_note/write_note/delete_note,
// which now write directly to the vault and require it be a git repo.
func gitVaultTools(t *testing.T) *Tools {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("cp", "-r", fixtureVault+"/.", root).CombinedOutput(); err != nil {
		t.Fatalf("cp fixture vault: %v: %s", err, out)
	}
	runTestGit(t, root, "init", "-q")
	runTestGit(t, root, "add", "-A")
	runTestGit(t, root, "commit", "-q", "-m", "seed")

	log := slog.New(slog.DiscardHandler)
	ix := vault.NewIndex(root, log)
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	se, err := search.New(root)
	if err != nil {
		t.Fatalf("search.New: %v", err)
	}
	return &Tools{Index: ix, Search: se, VaultRoot: root, MaxBodyBytes: 200_000, Log: log}
}

func runTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
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
	// soul.md carries no tier field, so it's dakhil by default — a title
	// match scores 1.0 before the tier weight (0.4 for dakhil) is applied.
	if len(titleOut.Results) != 1 || titleOut.Results[0].Path != "engineering/basim/soul.md" || titleOut.Results[0].Score != 0.4 {
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

func TestCreateWriteDeleteNoteLifecycle(t *testing.T) {
	tl := gitVaultTools(t)
	ctx := context.Background()

	created, err := tl.CreateNote(ctx, CreateNoteIn{
		Path: "drafts/idea.md", Frontmatter: map[string]any{"status": "draft"},
		Body: "# idea\n\nsomething worth keeping\n", Reason: "trying it out",
	})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if created.Path != "drafts/idea.md" {
		t.Fatalf("created.Path = %q", created.Path)
	}

	// immediately visible — no wait for the watcher
	got, err := tl.GetNote(ctx, GetNoteIn{Path: "drafts/idea.md"})
	if err != nil {
		t.Fatalf("GetNote right after CreateNote: %v", err)
	}
	if !strings.Contains(got.Body, "something worth keeping") {
		t.Fatalf("body = %q", got.Body)
	}

	// each write is its own commit
	if msg := runTestGit(t, tl.VaultRoot, "log", "-1", "--pretty=%B"); !strings.HasPrefix(msg, "Add drafts/idea.md") ||
		!strings.Contains(msg, "trying it out") || !strings.Contains(msg, "Source: nullmcp create_note") {
		t.Fatalf("commit message = %q", msg)
	}

	// creating over an existing note fails, with no new commit
	beforeCount := runTestGit(t, tl.VaultRoot, "rev-list", "--count", "HEAD")
	if _, err := tl.CreateNote(ctx, CreateNoteIn{Path: "drafts/idea.md", Body: "clobber"}); err == nil {
		t.Fatal("expected error creating over an existing note")
	}
	if after := runTestGit(t, tl.VaultRoot, "rev-list", "--count", "HEAD"); after != beforeCount {
		t.Fatalf("a rejected create must not commit: before=%s after=%s", beforeCount, after)
	}

	// write_note overwrites
	if _, err := tl.WriteNote(ctx, WriteNoteIn{
		Path: "drafts/idea.md", Body: "# idea\n\nrevised\n", Reason: "revising",
	}); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
	got, err = tl.GetNote(ctx, GetNoteIn{Path: "drafts/idea.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Body, "revised") {
		t.Fatalf("body after write = %q", got.Body)
	}
	if _, err := tl.WriteNote(ctx, WriteNoteIn{Path: "drafts/nope.md", Body: "x"}); err == nil {
		t.Fatal("expected error writing a note that doesn't exist")
	}

	// delete_note removes it, in its own commit
	if _, err := tl.DeleteNote(ctx, DeleteNoteIn{Path: "drafts/idea.md", Reason: "cleaning up"}); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if _, err := tl.GetNote(ctx, GetNoteIn{Path: "drafts/idea.md"}); err == nil {
		t.Fatal("expected error fetching a deleted note")
	}
	if msg := runTestGit(t, tl.VaultRoot, "log", "-1", "--pretty=%B"); !strings.HasPrefix(msg, "Delete drafts/idea.md") ||
		!strings.Contains(msg, "cleaning up") {
		t.Fatalf("delete commit message = %q", msg)
	}
	if _, err := tl.DeleteNote(ctx, DeleteNoteIn{Path: "drafts/idea.md"}); err == nil {
		t.Fatal("expected error deleting an already-deleted note")
	}

	// path safety still applies to every write tool
	if _, err := tl.CreateNote(ctx, CreateNoteIn{Path: "../../etc/passwd", Body: "x"}); err == nil {
		t.Fatal("expected error for a traversal attempt in create_note")
	}
}

func TestDeleteNoteErrorWrapping(t *testing.T) {
	tl := gitVaultTools(t)
	_, err := tl.DeleteNote(context.Background(), DeleteNoteIn{Path: "nope.md"})
	if err == nil || !strings.Contains(err.Error(), "no such note") {
		t.Fatalf("err = %v, want a clear 'no such note' message", err)
	}
}

// setTierOnDisk rewrites path's frontmatter tier directly on disk and
// commits it — standing in for the human git action that curates a
// note, since no MCP tool here can ever raise a tier (R1).
func setTierOnDisk(t *testing.T, root, path string, tier vault.Tier) {
	t.Helper()
	abs := root + "/" + path
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if rest, ok := strings.CutPrefix(body, "---\n"); ok {
		if _, after, found := strings.Cut(rest, "\n---\n"); found {
			body = after
		}
	}
	rewritten := "---\ntier: " + string(tier) + "\n---\n\n" + body
	if err := os.WriteFile(abs, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, root, "add", "-A")
	runTestGit(t, root, "commit", "-q", "-m", "curate "+path+" to "+string(tier))
}

func TestCreateNoteIgnoresModelSuppliedTier(t *testing.T) {
	tl := gitVaultTools(t)
	if _, err := tl.CreateNote(context.Background(), CreateNoteIn{
		Path:        "sneaky.md",
		Frontmatter: map[string]any{"tier": "asil"},
		Body:        "# sneaky\n",
	}); err != nil {
		t.Fatal(err)
	}
	n, ok := tl.Index.Get("sneaky.md")
	if !ok {
		t.Fatal("note not indexed")
	}
	if n.Tier != vault.TierDakhil {
		t.Fatalf("tier = %q, want dakhil — the model-supplied tier must be stripped, not honored", n.Tier)
	}
}

func TestWriteNoteDemotesThabitToAmil(t *testing.T) {
	tl := gitVaultTools(t)
	ctx := context.Background()
	if _, err := tl.CreateNote(ctx, CreateNoteIn{Path: "reviewed.md", Body: "# reviewed\n\nv1\n"}); err != nil {
		t.Fatal(err)
	}
	setTierOnDisk(t, tl.VaultRoot, "reviewed.md", vault.TierThabit)
	if err := tl.Index.Build(); err != nil {
		t.Fatal(err)
	}

	out, err := tl.WriteNote(ctx, WriteNoteIn{Path: "reviewed.md", Body: "# reviewed\n\nv2\n"})
	if err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
	if !out.Demoted {
		t.Fatal("Demoted = false, want true: editing a thabit note must demote it (R2)")
	}
	n, _ := tl.Index.Get("reviewed.md")
	if n.Tier != vault.TierAmil {
		t.Fatalf("tier after edit = %q, want amil — no window where it's edited and still thabit", n.Tier)
	}
}

func TestDeleteNoteForbiddenAboveDakhil(t *testing.T) {
	tl := gitVaultTools(t)
	ctx := context.Background()
	if _, err := tl.CreateNote(ctx, CreateNoteIn{Path: "curated.md", Body: "# curated\n"}); err != nil {
		t.Fatal(err)
	}
	setTierOnDisk(t, tl.VaultRoot, "curated.md", vault.TierAmil)
	if err := tl.Index.Build(); err != nil {
		t.Fatal(err)
	}

	if _, err := tl.DeleteNote(ctx, DeleteNoteIn{Path: "curated.md"}); err == nil {
		t.Fatal("expected error deleting a non-dakhil note")
	}
}

func TestWriteNoteRefusesAsilAtEveryBoundary(t *testing.T) {
	tl := gitVaultTools(t)
	ctx := context.Background()
	if _, err := tl.CreateNote(ctx, CreateNoteIn{Path: "foundational.md", Body: "# foundational\n"}); err != nil {
		t.Fatal(err)
	}
	setTierOnDisk(t, tl.VaultRoot, "foundational.md", vault.TierAsil)
	if err := tl.Index.Build(); err != nil {
		t.Fatal(err)
	}

	if _, err := tl.WriteNote(ctx, WriteNoteIn{Path: "foundational.md", Body: "# tampered\n"}); err == nil {
		t.Fatal("expected error writing an asil note")
	}
	if _, err := tl.DeleteNote(ctx, DeleteNoteIn{Path: "foundational.md"}); err == nil {
		t.Fatal("expected error deleting an asil note")
	}
	// even via a traversal-safe but differently-cased/relative form —
	// SafeRequestPath normalizes it to the same rel path either way
	if _, err := tl.WriteNote(ctx, WriteNoteIn{Path: "./foundational.md", Body: "# still tampered\n"}); err == nil {
		t.Fatal("expected error writing an asil note via a path that normalizes to it")
	}
}

func TestTierGetReflectsCurrentTierAndProposal(t *testing.T) {
	tl := gitVaultTools(t)
	ctx := context.Background()
	if _, err := tl.CreateNote(ctx, CreateNoteIn{Path: "watched.md", Body: "# watched\n"}); err != nil {
		t.Fatal(err)
	}

	got, err := tl.TierGet(ctx, TierGetIn{Path: "watched.md"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "dakhil" || got.Proposed != nil {
		t.Fatalf("got = %+v, want dakhil with no proposal", got)
	}

	if _, err := tl.TierPropose(ctx, TierProposeIn{Path: "watched.md", Tier: "amil", Reason: "worth surfacing"}); err != nil {
		t.Fatalf("TierPropose: %v", err)
	}
	tl.Index.Refresh("watched.md")

	got, err = tl.TierGet(ctx, TierGetIn{Path: "watched.md"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Proposed == nil || got.Proposed.Tier != "amil" || got.Proposed.Reason != "worth surfacing" {
		t.Fatalf("proposed = %+v, want amil/worth surfacing", got.Proposed)
	}
}

// TestTierSetEveryRaiseFails is R1 exercised at every tier boundary, not
// just one — spec/tiers.md's explicit test requirement.
func TestTierSetEveryRaiseFails(t *testing.T) {
	tl := gitVaultTools(t)
	ctx := context.Background()

	tests := []struct {
		start, attempt vault.Tier
	}{
		{vault.TierDakhil, vault.TierDakhil}, // hold, not a lower
		{vault.TierDakhil, vault.TierAmil},
		{vault.TierAmil, vault.TierThabit},
		{vault.TierThabit, vault.TierAsil},
	}
	for i, tt := range tests {
		path := fmt.Sprintf("raise-%d.md", i)
		if _, err := tl.CreateNote(ctx, CreateNoteIn{Path: path, Body: "# x\n"}); err != nil {
			t.Fatal(err)
		}
		if tt.start != vault.TierDakhil {
			setTierOnDisk(t, tl.VaultRoot, path, tt.start)
		}
		if err := tl.Index.Build(); err != nil {
			t.Fatal(err)
		}
		_, err := tl.TierSet(ctx, TierSetIn{Path: path, Tier: string(tt.attempt), Reason: "trying to raise"})
		if err == nil {
			t.Fatalf("case %d: tier_set from %s to %s should fail (raise or hold)", i, tt.start, tt.attempt)
		}
	}
}

func TestTierSetLowersAndRecordsHistory(t *testing.T) {
	tl := gitVaultTools(t)
	ctx := context.Background()
	if _, err := tl.CreateNote(ctx, CreateNoteIn{Path: "lower-me.md", Body: "# x\n"}); err != nil {
		t.Fatal(err)
	}
	setTierOnDisk(t, tl.VaultRoot, "lower-me.md", vault.TierThabit)
	if err := tl.Index.Build(); err != nil {
		t.Fatal(err)
	}

	if _, err := tl.TierSet(ctx, TierSetIn{Path: "lower-me.md", Tier: "dakhil", Reason: "was miscurated"}); err != nil {
		t.Fatalf("TierSet: %v", err)
	}
	tl.Index.Refresh("lower-me.md")

	n, _ := tl.Index.Get("lower-me.md")
	if n.Tier != vault.TierDakhil {
		t.Fatalf("tier = %q, want dakhil", n.Tier)
	}
	history, _ := n.Frontmatter["tier_history"].([]any)
	if len(history) != 1 {
		t.Fatalf("tier_history = %v, want exactly one entry", history)
	}
}

func TestTierProposeRefusesUnexpiredDenial(t *testing.T) {
	tl := gitVaultTools(t)
	ctx := context.Background()
	if _, err := tl.CreateNote(ctx, CreateNoteIn{Path: "denied.md", Body: "# x\n"}); err != nil {
		t.Fatal(err)
	}

	// simulate Al-Mina having denied a prior proposal to amil
	abs := tl.VaultRoot + "/denied.md"
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	deniedAt := time.Now().Add(-time.Hour)
	rewritten := fmt.Sprintf("---\ndenied_tier: amil\ndenied_reason: not yet\ndenied_at: %s\n---\n\n%s",
		deniedAt.UTC().Format(time.RFC3339), string(data))
	if err := os.WriteFile(abs, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, tl.VaultRoot, "add", "-A")
	runTestGit(t, tl.VaultRoot, "commit", "-q", "-m", "deny proposal")
	if err := os.Chtimes(abs, deniedAt, deniedAt); err != nil {
		t.Fatal(err)
	}
	if err := tl.Index.Build(); err != nil {
		t.Fatal(err)
	}

	if _, err := tl.TierPropose(ctx, TierProposeIn{Path: "denied.md", Tier: "amil", Reason: "asking again"}); err == nil {
		t.Fatal("expected the unexpired denial to refuse the proposal")
	}

	// editing the note since the denial clears the way
	if _, err := tl.WriteNote(ctx, WriteNoteIn{Path: "denied.md", Body: "# x\n\nnow with more content\n"}); err != nil {
		t.Fatal(err)
	}
	tl.Index.Refresh("denied.md")
	if _, err := tl.TierPropose(ctx, TierProposeIn{Path: "denied.md", Tier: "amil", Reason: "asking again, note has changed"}); err != nil {
		t.Fatalf("TierPropose after an edit should succeed: %v", err)
	}
}
