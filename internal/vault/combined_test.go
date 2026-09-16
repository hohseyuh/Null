package vault

import (
	"log/slog"
	"testing"
)

// buildCombined sets up a primary index over the fixture vault and a
// secondary index over a fresh temp dir seeded with the given inbox
// files, merged under the "inbox/" prefix.
func buildCombined(t *testing.T, inboxFiles map[string]string) *Combined {
	t.Helper()
	log := slog.New(slog.DiscardHandler)

	primary := NewIndex(fixtureVault, log)
	if err := primary.Build(); err != nil {
		t.Fatal(err)
	}

	inboxRoot := t.TempDir()
	for rel, content := range inboxFiles {
		if err := CreateNote(inboxRoot, rel, nil, content); err != nil {
			t.Fatal(err)
		}
	}
	secondary := NewIndex(inboxRoot, log)
	secondary.Source = SourceInbox
	if err := secondary.Build(); err != nil {
		t.Fatal(err)
	}

	return NewCombined(primary, secondary, "inbox/")
}

func TestCombinedGetAndSource(t *testing.T) {
	c := buildCombined(t, map[string]string{"thought.md": "# thought\n\na draft\n"})

	v, ok := c.Get("engineering/basim/soul.md")
	if !ok || v.Source != SourceVault {
		t.Fatalf("vault note: ok=%v source=%q", ok, v.Source)
	}

	i, ok := c.Get("inbox/thought.md")
	if !ok || i.Source != SourceInbox || i.Path != "inbox/thought.md" {
		t.Fatalf("inbox note: ok=%v source=%q path=%q", ok, i.Source, i.Path)
	}

	if _, ok := c.Get("thought.md"); ok {
		t.Fatal("inbox note reachable without its prefix — namespace collision risk")
	}
}

func TestCombinedAllMergesBoth(t *testing.T) {
	c := buildCombined(t, map[string]string{"a.md": "# a\n", "b.md": "# b\n"})
	all := c.All()
	if len(all) != 8 { // 6 fixture vault notes + 2 inbox notes
		t.Fatalf("got %d notes, want 8", len(all))
	}
	var inboxSeen, vaultSeen int
	for _, n := range all {
		switch n.Source {
		case SourceInbox:
			inboxSeen++
		case SourceVault:
			vaultSeen++
		default:
			t.Fatalf("note %s has no source", n.Path)
		}
	}
	if inboxSeen != 2 || vaultSeen != 6 {
		t.Fatalf("inboxSeen=%d vaultSeen=%d", inboxSeen, vaultSeen)
	}
	if c.Len() != 8 {
		t.Fatalf("Len() = %d, want 8", c.Len())
	}
}

func TestCombinedGraphAndBacklinksStayWithinOneSide(t *testing.T) {
	c := buildCombined(t, map[string]string{
		"a.md": "# a\n\nlinks to [[b]]\n",
		"b.md": "# b\n",
	})

	edges := c.Backlinks("inbox/b.md")
	if len(edges) != 1 || edges[0].From != "inbox/a.md" || edges[0].To != "inbox/b.md" {
		t.Fatalf("inbox backlinks not prefixed correctly: %+v", edges)
	}

	g := c.Graph("inbox/a.md", 1, "out")
	if g.Root != "inbox/a.md" || len(g.Nodes) != 1 || g.Nodes[0].Path != "inbox/b.md" {
		t.Fatalf("inbox graph not prefixed correctly: root=%s nodes=%+v", g.Root, g.Nodes)
	}

	// an inbox note linking to a real vault note does not cross the
	// boundary in this v0 — documented limitation, asserted so a future
	// change to this behavior is a deliberate one.
	cross := buildCombined(t, map[string]string{"c.md": "# c\n\nsee [[soul]]\n"})
	if backs := cross.Backlinks("engineering/basim/soul.md"); len(backs) != 3 {
		t.Fatalf("cross-boundary link leaked into vault backlinks: %+v", backs)
	}
}

func TestCombinedResolveCrossesBoundary(t *testing.T) {
	c := buildCombined(t, map[string]string{"c.md": "# c\n"})

	// resolving a vault-side target still works
	if p, ok := c.Resolve("soul"); !ok || p != "engineering/basim/soul.md" {
		t.Fatalf("Resolve(soul) = %q, %v", p, ok)
	}
	// resolving an inbox-side target returns the prefixed path
	if p, ok := c.Resolve("c"); !ok || p != "inbox/c.md" {
		t.Fatalf("Resolve(c) = %q, %v", p, ok)
	}
	if _, ok := c.Resolve("does-not-exist-anywhere"); ok {
		t.Fatal("expected no resolution")
	}
}
