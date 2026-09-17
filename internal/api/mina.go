package api

import (
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"null-service/internal/vault"
)

// minaStaleDakhilDays is how old a dakhil note must be, with no
// outstanding proposal, before it lands in Al-Mina as a staleness sweep
// entry — "the query that replaces tier_expire" (spec/tiers.md).
// Overridable via NULL_MINA_STALE_DAYS; no single default suits every
// vault's capture rate.
const minaStaleDakhilDaysDefault = 14

func minaStaleDakhilDays() int {
	if v := os.Getenv("NULL_MINA_STALE_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return minaStaleDakhilDaysDefault
}

// minaEntry is one row in Al-Mina's queue: either a note with an
// outstanding proposal, or a dakhil note stale enough to prompt a look.
type minaEntry struct {
	Path               string  `json:"path"`
	Title              string  `json:"title"`
	Type               string  `json:"type"` // "proposal" or "stale_dakhil"
	Tier               string  `json:"tier"`
	ProposedTier       string  `json:"proposed_tier,omitempty"`
	Reason             string  `json:"reason,omitempty"`
	AgeDays            float64 `json:"age_days"`
	ChangedSinceReview bool    `json:"changed_since_review,omitempty"`
}

// handleMina serves GET /mina: the Al-Mina proposal queue — every note
// with an outstanding proposal, plus dakhil notes stale enough for a
// sweep. Read-only: it is a query over frontmatter the index already
// holds, not a write. Approving, denying, and deferring an entry is a
// human action taken in the renderer (M7); this endpoint only surfaces
// what's waiting.
func (s *Server) handleMina(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	staleCutoff := now.AddDate(0, 0, -minaStaleDakhilDays())

	var entries []minaEntry
	for _, n := range s.Index.All() {
		if p := vault.ProposalFrom(n.Frontmatter); p != nil {
			entries = append(entries, minaEntry{
				Path: n.Path, Title: n.Title, Type: "proposal",
				Tier: string(n.Tier), ProposedTier: string(p.Tier), Reason: p.Reason,
				AgeDays:            ageDays(now, p.At),
				ChangedSinceReview: changedSinceLastThabit(n),
			})
			continue
		}
		if n.Tier == vault.TierDakhil && n.UpdatedAt.Before(staleCutoff) {
			entries = append(entries, minaEntry{
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
	if entries == nil {
		entries = []minaEntry{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func ageDays(now, at time.Time) float64 {
	if at.IsZero() {
		return 0
	}
	return now.Sub(at).Hours() / 24
}

// changedSinceLastThabit reports whether n's tier_history shows it left
// thabit (an R2 demotion) more recently than it last entered thabit —
// the simplified stand-in for a full body diff: it answers "has this
// note moved since it was last reviewed" without shelling out to git log
// -p for every entry in the queue.
func changedSinceLastThabit(n *vault.Note) bool {
	history, _ := n.Frontmatter["tier_history"].([]any)
	var lastEnteredThabit, lastLeftThabit time.Time
	for _, raw := range history {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		to, _ := entry["to"].(string)
		from, _ := entry["from"].(string)
		at := parseHistoryTime(entry["at"])
		if to == string(vault.TierThabit) {
			lastEnteredThabit = at
		}
		if from == string(vault.TierThabit) {
			lastLeftThabit = at
		}
	}
	return lastLeftThabit.After(lastEnteredThabit)
}

func parseHistoryTime(v any) time.Time {
	s, ok := v.(string)
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
