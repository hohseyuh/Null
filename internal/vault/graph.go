package vault

import "sort"

// GraphNode is one note reached by a graph traversal, with its BFS
// distance from the root.
type GraphNode struct {
	Path     string `json:"path"`
	Title    string `json:"title"`
	Distance int    `json:"distance"`
	Source   string `json:"source"`
}

// GraphResult is the neighborhood of one note. Nodes exclude the root
// (it is named separately); Edges carry the context line each wikilink
// appeared on — the reason /graph exists.
type GraphResult struct {
	Root  string
	Nodes []GraphNode
	Edges []Edge
}

// Graph traverses the link graph from root by BFS up to depth hops.
// direction is "out", "in", or "both". It is cycle-safe (visited set) and
// deduplicates nodes by path at their first (shortest) distance and edges
// by (from, to, line). It assumes root is an indexed path — callers check
// existence first — and depth >= 1.
func (ix *Index) Graph(root string, depth int, direction string) GraphResult {
	res := GraphResult{Root: root, Nodes: []GraphNode{}, Edges: []Edge{}}

	visited := map[string]int{root: 0}
	frontier := []string{root}
	type edgeKey struct {
		from, to string
		line     int
	}
	seenEdge := map[edgeKey]struct{}{}

	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, p := range frontier {
			var edges []Edge
			if direction == "out" || direction == "both" {
				edges = append(edges, ix.Outlinks(p)...)
			}
			if direction == "in" || direction == "both" {
				edges = append(edges, ix.Backlinks(p)...)
			}
			for _, e := range edges {
				k := edgeKey{e.From, e.To, e.Line}
				if _, dup := seenEdge[k]; dup {
					continue
				}
				seenEdge[k] = struct{}{}
				res.Edges = append(res.Edges, e)

				// the far endpoint relative to p
				other := e.To
				if other == p {
					other = e.From
				}
				if _, seen := visited[other]; !seen {
					visited[other] = d + 1
					next = append(next, other)
				}
			}
		}
		frontier = next
	}

	ix.mu.RLock()
	for p, dist := range visited {
		if p == root {
			continue
		}
		title, source := "", ""
		if n, ok := ix.notes[p]; ok {
			title, source = n.Title, n.Source
		}
		res.Nodes = append(res.Nodes, GraphNode{Path: p, Title: title, Distance: dist, Source: source})
	}
	ix.mu.RUnlock()

	sort.Slice(res.Nodes, func(i, j int) bool {
		if res.Nodes[i].Distance != res.Nodes[j].Distance {
			return res.Nodes[i].Distance < res.Nodes[j].Distance
		}
		return res.Nodes[i].Path < res.Nodes[j].Path
	})
	sort.Slice(res.Edges, func(i, j int) bool {
		if res.Edges[i].From != res.Edges[j].From {
			return res.Edges[i].From < res.Edges[j].From
		}
		if res.Edges[i].To != res.Edges[j].To {
			return res.Edges[i].To < res.Edges[j].To
		}
		return res.Edges[i].Line < res.Edges[j].Line
	})
	return res
}
