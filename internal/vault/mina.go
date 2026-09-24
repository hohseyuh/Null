package vault

import (
	"os"
	"sort"
	"strconv"
	"time"
)

// MinaStaleDaysDefault is how old a dakhil note must be, with no
// outstanding proposal, before it lands in Al-Mina as a staleness sweep
// entry — "the query that replaces tier_expire" (spec/tiers.md).
const MinaStaleDaysDefault = 14

// MinaStaleDays returns the staleness threshold: NULL_MINA_STALE_DAYS if
// it is a positive integer, else MinaStaleDaysDefault. No single default
// suits every vault's capture rate.
func MinaStaleDays() int {
	if v := os.Getenv("NULL_MINA_STALE_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return MinaStaleDaysDefault
}

// MinaEntry is one row in Al-Mina's queue: either a note with an
// outstanding proposal ("proposal"), or a dakhil note stale enough to
// prompt a look ("stale_dakhil").
type MinaEntry struct {
	Path               string  `json:"path"`
	Title              string  `json:"title"`
	Type               string  `json:"type"`
	Tier               string  `json:"tier"`
	ProposedTier       string  `json:"proposed_tier,omitempty"`
	Reason             string  `json:"reason,omitempty"`
	AgeDays            float64 `json:"age_days"`
	ChangedSinceReview bool    `json:"changed_since_review,omitempty"`
}

// MinaQueue computes Al-Mina's queue over ix as of now: every note with
// an outstanding proposal, plus dakhil notes older than staleDays with no
// proposal. Oldest first. Read-only — a query over frontmatter the index
// already holds. Returns a non-nil slice.
func MinaQueue(ix *Index, staleDays int, now time.Time) []MinaEntry {
	cutoff := now.AddDate(0, 0, -staleDays)
	entries := []MinaEntry{}
	for _, n := range ix.All() {
		if p := ProposalFrom(n.Frontmatter); p != nil {
			entries = append(entries, MinaEntry{
				Path: n.Path, Title: n.Title, Type: "proposal",
				Tier: string(n.Tier), ProposedTier: string(p.Tier), Reason: p.Reason,
				AgeDays:            ageDays(now, p.At),
				ChangedSinceReview: changedSinceLastThabit(n),
			})
			continue
		}
		if n.Tier == TierDakhil && n.UpdatedAt.Before(cutoff) {
			entries = append(entries, MinaEntry{
				Path: n.Path, Title: n.Title, Type: "stale_dakhil",
				Tier: string(n.Tier), AgeDays: ageDays(now, n.UpdatedAt),
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].AgeDays != entries[j].AgeDays {
			return entries[i].AgeDays > entries[j].AgeDays
		}
		return entries[i].Path < entries[j].Path
	})
	return entries
}

func ageDays(now, at time.Time) float64 {
	if at.IsZero() {
		return 0
	}
	return now.Sub(at).Hours() / 24
}

// changedSinceLastThabit reports whether n's tier_history shows it left
// thabit (an R2 demotion) more recently than it last entered thabit — the
// simplified stand-in for a full body diff: it answers "has this note
// moved since it was last reviewed" without shelling out to git log -p
// for every entry in the queue.
func changedSinceLastThabit(n *Note) bool {
	history, _ := n.Frontmatter["tier_history"].([]any)
	var enteredThabit, leftThabit time.Time
	for _, raw := range history {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		to, _ := entry["to"].(string)
		from, _ := entry["from"].(string)
		at := parseFrontmatterTime(entry["at"])
		if to == string(TierThabit) {
			enteredThabit = at
		}
		if from == string(TierThabit) {
			leftThabit = at
		}
	}
	return leftThabit.After(enteredThabit)
}

// TierCounts returns how many indexed notes sit at each tier — the
// accumulation check shown in the renderer header. Every tier is present
// in the result, zero included.
func (ix *Index) TierCounts() map[Tier]int {
	counts := map[Tier]int{TierDakhil: 0, TierAmil: 0, TierThabit: 0, TierAsil: 0}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	for _, n := range ix.notes {
		counts[n.Tier]++
	}
	return counts
}

// WholeGraphNode is one note in the whole-vault graph.
type WholeGraphNode struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Tier  Tier   `json:"tier"`
}

// WholeGraphEdge is one distinct resolved link between two notes,
// carrying the lower of its endpoints' tiers. Multiple wikilinks between
// the same ordered pair collapse into one edge — this view is for shape,
// not per-line context (that is /graph's job).
type WholeGraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Tier Tier   `json:"tier"`
}

// WholeGraph returns every note and every distinct resolved link — the
// renderer's graph view. Nothing is excluded by tier. Nodes are sorted by
// path, edges by (from, to).
func (ix *Index) WholeGraph() ([]WholeGraphNode, []WholeGraphEdge) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	nodes := make([]WholeGraphNode, 0, len(ix.notes))
	for _, n := range ix.notes {
		nodes = append(nodes, WholeGraphNode{Path: n.Path, Title: n.Title, Tier: n.Tier})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Path < nodes[j].Path })

	type pair struct{ from, to string }
	seen := map[pair]struct{}{}
	edges := []WholeGraphEdge{}
	for _, n := range ix.notes {
		for _, l := range n.Outlinks {
			if !l.Resolved || l.Path == n.Path {
				continue
			}
			k := pair{n.Path, l.Path}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			target, ok := ix.notes[l.Path]
			if !ok {
				continue
			}
			edges = append(edges, WholeGraphEdge{From: n.Path, To: l.Path, Tier: LowerOf(n.Tier, target.Tier)})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	return nodes, edges
}
