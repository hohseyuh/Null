package vault

import "testing"

func TestGraphTwoHopOut(t *testing.T) {
	ix := buildFixtureIndex(t)
	g := ix.Graph("engineering/basim/character.md", 2, "out")

	// character -> soul (hop 1), soul -> {character, index} (hop 2)
	wantNodes := map[string]int{
		"engineering/basim/soul.md": 1,
		"index.md":                  2,
	}
	if len(g.Nodes) != len(wantNodes) {
		t.Fatalf("nodes = %+v", g.Nodes)
	}
	for _, n := range g.Nodes {
		if wantNodes[n.Path] != n.Distance {
			t.Fatalf("node %s distance = %d, want %d", n.Path, n.Distance, wantNodes[n.Path])
		}
		if n.Title == "" {
			t.Fatalf("node %s has no title", n.Path)
		}
	}

	// 4 edges: character->soul, soul->character x2 (two lines), soul->index
	if len(g.Edges) != 4 {
		t.Fatalf("edges = %+v", g.Edges)
	}
	for _, e := range g.Edges {
		if e.Context == "" {
			t.Fatalf("edge %s -> %s has no context line", e.From, e.To)
		}
	}
	// the soul->character cycle must not loop or duplicate
	seen := map[string]int{}
	for _, e := range g.Edges {
		seen[e.From+"->"+e.To]++
	}
	if seen["engineering/basim/character.md->engineering/basim/soul.md"] != 1 ||
		seen["engineering/basim/soul.md->engineering/basim/character.md"] != 2 {
		t.Fatalf("edge multiset wrong: %v", seen)
	}
}

func TestGraphDirections(t *testing.T) {
	ix := buildFixtureIndex(t)

	out := ix.Graph("engineering/basim/soul.md", 1, "out")
	if len(out.Nodes) != 2 { // character, index
		t.Fatalf("out nodes = %+v", out.Nodes)
	}

	in := ix.Graph("engineering/basim/soul.md", 1, "in")
	if len(in.Nodes) != 3 { // character, barzakh, index
		t.Fatalf("in nodes = %+v", in.Nodes)
	}

	both := ix.Graph("engineering/basim/soul.md", 1, "both")
	if len(both.Nodes) != 3 || len(both.Edges) != 6 {
		t.Fatalf("both: nodes = %+v, edges = %+v", both.Nodes, both.Edges)
	}
}

func TestGraphUnresolvedLinksAbsent(t *testing.T) {
	ix := buildFixtureIndex(t)
	g := ix.Graph("plain.md", 3, "both")
	for _, n := range g.Nodes {
		if n.Path == "does-not-exist.md" {
			t.Fatal("unresolved link surfaced as a node")
		}
	}
}
