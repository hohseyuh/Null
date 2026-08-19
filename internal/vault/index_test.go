package vault

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func buildFixtureIndex(t *testing.T) *Index {
	t.Helper()
	ix := NewIndex(fixtureVault, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return ix
}

func TestIndexBuild(t *testing.T) {
	ix := buildFixtureIndex(t)

	if got, want := ix.Len(), len(fixturePaths); got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
	if _, ok := ix.Get(".hidden/secret.md"); ok {
		t.Fatal("dot-directory note was indexed")
	}

	// resolution ran: soul.md's alias link resolved to character.md
	soul, ok := ix.Get("engineering/basim/soul.md")
	if !ok {
		t.Fatal("soul.md missing from index")
	}
	if l := soul.Outlinks[0]; !l.Resolved || l.Path != "engineering/basim/character.md" {
		t.Fatalf("outlink not resolved: %+v", l)
	}

	// backlinks: soul.md is linked from character.md, barzakh.md, index.md
	backs := ix.Backlinks("engineering/basim/soul.md")
	var froms []string
	for _, e := range backs {
		froms = append(froms, e.From)
		if e.Context == "" {
			t.Errorf("edge %s -> %s has no context", e.From, e.To)
		}
	}
	want := []string{"engineering/basim/character.md", "index.md", "philosophy/barzakh.md"}
	if len(froms) != len(want) {
		t.Fatalf("backlink froms = %v, want %v", froms, want)
	}
	for i := range want {
		if froms[i] != want[i] {
			t.Fatalf("backlink froms = %v, want %v", froms, want)
		}
	}

	// broken link contributes no edges
	if backs := ix.Backlinks("does-not-exist.md"); len(backs) != 0 {
		t.Fatalf("broken link produced backlinks: %v", backs)
	}
}

func TestIndexOutlinksExcludeUnresolved(t *testing.T) {
	ix := buildFixtureIndex(t)
	edges := ix.Outlinks("plain.md")
	if len(edges) != 0 {
		t.Fatalf("plain.md has only a broken link, got edges %v", edges)
	}
}

// writeVaultFile is a test helper; the service itself never writes.
func writeVaultFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyBatchAddUpdateDelete(t *testing.T) {
	root := t.TempDir()
	writeVaultFile(t, root, "a.md", "# a\n\nlinks to [[b]]\n")
	writeVaultFile(t, root, "b.md", "# b\n")

	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	if len(ix.Backlinks("b.md")) != 1 {
		t.Fatal("expected a.md -> b.md backlink")
	}

	// add: c.md linking to b.md
	writeVaultFile(t, root, "c.md", "# c\n\nalso [[b]]\n")
	ix.applyBatch([]string{"c.md"})
	if len(ix.Backlinks("b.md")) != 2 {
		t.Fatal("expected two backlinks after add")
	}

	// update: a.md no longer links to b.md
	writeVaultFile(t, root, "a.md", "# a\n\nno links now\n")
	ix.applyBatch([]string{"a.md"})
	if got := ix.Backlinks("b.md"); len(got) != 1 || got[0].From != "c.md" {
		t.Fatalf("stale edge after update: %v", got)
	}

	// delete: b.md gone, dangling edges cleaned, c's link unresolved
	if err := os.Remove(filepath.Join(root, "b.md")); err != nil {
		t.Fatal(err)
	}
	ix.applyBatch([]string{"b.md"})
	if _, ok := ix.Get("b.md"); ok {
		t.Fatal("b.md still indexed after delete")
	}
	c, _ := ix.Get("c.md")
	if c.Outlinks[0].Resolved {
		t.Fatal("link to deleted note still resolved")
	}
}
