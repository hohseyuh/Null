package vault

import "time"

// Tier is a note's ordinal curation level: a claim about provenance and
// review status, never about truth. Stored as frontmatter tier. See
// spec/tiers.md and epistemics.md for the full semantics; this file is
// only the mechanism.
type Tier string

const (
	TierDakhil Tier = "dakhil" // model-created, uncurated; the default
	TierAmil   Tier = "amil"   // user aware of it, in progress
	TierThabit Tier = "thabit" // curated, reviewed, proven
	TierAsil   Tier = "asil"   // foundational, immutable to the model
)

var tierRank = map[Tier]int{TierDakhil: 1, TierAmil: 2, TierThabit: 3, TierAsil: 4}

// searchWeight is the suggested starting multiplier applied to a search
// hit's score by tier — settled knowledge outranks raw capture without
// excluding it. Tune from the misses log, not from intuition.
var searchWeight = map[Tier]float64{TierDakhil: 0.4, TierAmil: 0.8, TierThabit: 1.0, TierAsil: 1.0}

// ParseTier reads a frontmatter tier value, defaulting to TierDakhil for
// anything missing, non-string, or unrecognized. Never guesses a higher
// default than dakhil — an invalid value is treated exactly like an
// absent one, not an error, the same way malformed frontmatter degrades
// rather than failing the whole note.
func ParseTier(v any) Tier {
	s, _ := v.(string)
	if t := Tier(s); t.Valid() {
		return t
	}
	return TierDakhil
}

// Valid reports whether t is one of the four known tiers.
func (t Tier) Valid() bool {
	_, ok := tierRank[t]
	return ok
}

// Rank returns t's ordinal position, 1 (dakhil) through 4 (asil). An
// invalid Tier ranks 0, below dakhil, so it never accidentally compares
// as already-highest.
func (t Tier) Rank() int { return tierRank[t] }

// SearchWeight returns the multiplier search ranking applies for t.
func (t Tier) SearchWeight() float64 { return searchWeight[t] }

// LowerOf returns whichever of a, b has the lower rank — used to weight a
// graph edge by its weaker endpoint, so an edge is only as trustworthy as
// the less-reviewed note it touches.
func LowerOf(a, b Tier) Tier {
	if a.Rank() <= b.Rank() {
		return a
	}
	return b
}

// TierProposal is an outstanding proposed_* trio read from a note's
// frontmatter — Basim asking the user to raise a tier, via Al-Mina.
type TierProposal struct {
	Tier   Tier      `json:"tier"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// TierDenial is a denied_* trio: a proposal the user rejected, and why —
// data, not silence, so the same proposal doesn't keep resurfacing.
type TierDenial struct {
	Tier   Tier      `json:"tier"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// serverOwnedFrontmatterKeys are never trusted from a model write. If a
// model write includes any of them, the caller strips them and the server
// writes its own values — silently authoritative, not an error, since the
// model has no legitimate reason to set them and no feedback loop to
// learn from (see spec/tiers.md, "Enforcement: server-owned, not
// model-observed").
var serverOwnedFrontmatterKeys = []string{
	"tier",
	"proposed_tier", "proposed_reason", "proposed_at",
	"denied_tier", "denied_reason", "denied_at",
	"tier_history",
}

// stripServerOwned returns a copy of fm with every server-owned key
// removed, leaving the caller's own frontmatter untouched to mutate
// freely. A nil fm returns an empty, non-nil map.
func stripServerOwned(fm map[string]any) map[string]any {
	out := make(map[string]any, len(fm))
	for k, v := range fm {
		if slicesContainsString(serverOwnedFrontmatterKeys, k) {
			continue
		}
		out[k] = v
	}
	return out
}

func slicesContainsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// timeString formats t for frontmatter storage — RFC 3339, the same
// format every other timestamp in this codebase's wire format uses.
func timeString(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parseFrontmatterTime reads an RFC 3339 timestamp out of a frontmatter
// value, returning the zero Time if v isn't a parseable string.
func parseFrontmatterTime(v any) time.Time {
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

// ProposalFrom reads an outstanding proposal out of frontmatter, if any.
func ProposalFrom(fm map[string]any) *TierProposal {
	v, ok := fm["proposed_tier"]
	if !ok {
		return nil
	}
	t := Tier(fmtString(v))
	if !t.Valid() {
		return nil
	}
	reason, _ := fm["proposed_reason"].(string)
	return &TierProposal{Tier: t, Reason: reason, At: parseFrontmatterTime(fm["proposed_at"])}
}

// DenialFrom reads a recorded denial out of frontmatter, if any.
func DenialFrom(fm map[string]any) *TierDenial {
	v, ok := fm["denied_tier"]
	if !ok {
		return nil
	}
	t := Tier(fmtString(v))
	if !t.Valid() {
		return nil
	}
	reason, _ := fm["denied_reason"].(string)
	return &TierDenial{Tier: t, Reason: reason, At: parseFrontmatterTime(fm["denied_at"])}
}

func fmtString(v any) string {
	s, _ := v.(string)
	return s
}
