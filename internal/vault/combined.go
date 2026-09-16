package vault

import "strings"

// Combined presents two Readers — a primary (the vault) and a secondary
// (the inbox) — as one merged read surface, addressed by a single path
// space. Secondary paths are exposed prefixed by Prefix (e.g. "inbox/")
// so they can never collide with the primary's namespace; this is the
// one constraint it places on vault layout — the vault must not itself
// have a top-level directory literally named the same as Prefix, the
// same kind of reserved name ".git/" already is.
//
// Known v0 limitation, documented rather than hidden: link resolution,
// backlinks, and Graph traversal do not cross the primary/secondary
// boundary. Each underlying Reader resolves links only within its own
// note set, so a wikilink from an inbox draft to a real vault note (or
// vice versa) stays unresolved in Outlinks/Backlinks/Graph until the
// note is promoted into the vault — at which point normal resolution
// takes over, because promotion is a real filesystem move into the one
// root that Reader actually indexes. Resolve is the one exception: it
// tries both sides, so a rendered link still points somewhere valid
// even though the corresponding graph edge does not yet exist.
type Combined struct {
	primary   Reader
	secondary Reader
	prefix    string
}

// NewCombined merges primary and secondary into one Reader. prefix must
// be non-empty and is prepended to every secondary path.
func NewCombined(primary, secondary Reader, prefix string) *Combined {
	return &Combined{primary: primary, secondary: secondary, prefix: prefix}
}

// route decides which underlying Reader a merged path belongs to and
// returns the path relative to that Reader.
func (c *Combined) route(path string) (r Reader, rel string, fromSecondary bool) {
	if rel, ok := strings.CutPrefix(path, c.prefix); ok {
		return c.secondary, rel, true
	}
	return c.primary, path, false
}

// rewrap returns a copy of n with Path set to its merged (prefixed)
// address. Callers must never mutate the notes an Index hands out, so
// this always copies rather than assigning in place.
func (c *Combined) rewrap(n *Note) *Note {
	if n == nil {
		return nil
	}
	cp := *n
	cp.Path = c.prefix + n.Path
	return &cp
}

func (c *Combined) rewrapEdges(edges []Edge, fromSecondary bool) []Edge {
	if !fromSecondary || len(edges) == 0 {
		return edges
	}
	out := make([]Edge, len(edges))
	for i, e := range edges {
		out[i] = Edge{From: c.prefix + e.From, To: c.prefix + e.To, Context: e.Context, Line: e.Line}
	}
	return out
}

// Get returns the note at a merged path, from whichever side owns it.
func (c *Combined) Get(path string) (*Note, bool) {
	r, rel, secondary := c.route(path)
	n, ok := r.Get(rel)
	if !ok {
		return nil, false
	}
	if secondary {
		return c.rewrap(n), true
	}
	return n, true
}

// All returns every note from both sides, primary paths unchanged and
// secondary paths prefixed.
func (c *Combined) All() []*Note {
	primary := c.primary.All()
	secondary := c.secondary.All()
	out := make([]*Note, 0, len(primary)+len(secondary))
	out = append(out, primary...)
	for _, n := range secondary {
		out = append(out, c.rewrap(n))
	}
	return out
}

// Backlinks returns the edges pointing at a merged path. Per the package
// doc, this never crosses the primary/secondary boundary.
func (c *Combined) Backlinks(path string) []Edge {
	r, rel, secondary := c.route(path)
	return c.rewrapEdges(r.Backlinks(rel), secondary)
}

// Outlinks returns the resolved outgoing edges of a merged path. Per the
// package doc, this never crosses the primary/secondary boundary.
func (c *Combined) Outlinks(path string) []Edge {
	r, rel, secondary := c.route(path)
	return c.rewrapEdges(r.Outlinks(rel), secondary)
}

// Graph traverses within whichever side owns root. Per the package doc,
// the traversal never crosses the primary/secondary boundary.
func (c *Combined) Graph(root string, depth int, direction string) GraphResult {
	r, rel, secondary := c.route(root)
	g := r.Graph(rel, depth, direction)
	if !secondary {
		return g
	}
	out := GraphResult{Root: c.prefix + g.Root, Nodes: make([]GraphNode, len(g.Nodes))}
	for i, n := range g.Nodes {
		out.Nodes[i] = GraphNode{Path: c.prefix + n.Path, Title: n.Title, Distance: n.Distance, Source: n.Source}
	}
	out.Edges = c.rewrapEdges(g.Edges, true)
	return out
}

// Resolve tries the primary first, then the secondary (prefixing a
// secondary hit). This is the one method allowed to cross the boundary —
// see the package doc for why that's safe here but not for graph edges.
func (c *Combined) Resolve(target string) (string, bool) {
	if p, ok := c.primary.Resolve(target); ok {
		return p, true
	}
	if p, ok := c.secondary.Resolve(target); ok {
		return c.prefix + p, true
	}
	return "", false
}

// Len returns the total note count across both sides.
func (c *Combined) Len() int {
	return c.primary.Len() + c.secondary.Len()
}
