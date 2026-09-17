package vault

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseTierDefaultsToDakhil(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want Tier
	}{
		{"missing", nil, TierDakhil},
		{"empty string", "", TierDakhil},
		{"garbage", "made-up-tier", TierDakhil},
		{"wrong type", 4, TierDakhil},
		{"dakhil", "dakhil", TierDakhil},
		{"amil", "amil", TierAmil},
		{"thabit", "thabit", TierThabit},
		{"asil", "asil", TierAsil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseTier(tt.v); got != tt.want {
				t.Fatalf("ParseTier(%v) = %q, want %q", tt.v, got, tt.want)
			}
		})
	}
}

func TestTierRankOrdering(t *testing.T) {
	if !(TierDakhil.Rank() < TierAmil.Rank() && TierAmil.Rank() < TierThabit.Rank() && TierThabit.Rank() < TierAsil.Rank()) {
		t.Fatalf("tier ranks not strictly increasing: dakhil=%d amil=%d thabit=%d asil=%d",
			TierDakhil.Rank(), TierAmil.Rank(), TierThabit.Rank(), TierAsil.Rank())
	}
}

func TestLowerOf(t *testing.T) {
	if LowerOf(TierAsil, TierDakhil) != TierDakhil {
		t.Fatal("LowerOf should pick the weaker endpoint regardless of argument order")
	}
	if LowerOf(TierDakhil, TierAsil) != TierDakhil {
		t.Fatal("LowerOf should be symmetric")
	}
}

// TestNoTierFieldIndexesAsDakhil is one of spec/tiers.md's explicit
// required tests.
func TestNoTierFieldIndexesAsDakhil(t *testing.T) {
	n := Parse("plain.md", []byte("# plain\n\nno frontmatter at all\n"), time.Now(), slog.New(slog.DiscardHandler))
	if n.Tier != TierDakhil {
		t.Fatalf("tier = %q, want dakhil", n.Tier)
	}
}

// TestModelWriteCannotSetServerOwnedFields is spec/tiers.md's explicit
// required test: a model write containing tier, proposed_*, or denied_*
// has those fields stripped, and the server's own values survive.
func TestModelWriteCannotSetServerOwnedFields(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "sneaky.md", map[string]any{
		"tier": "asil", "proposed_tier": "asil", "denied_tier": "amil",
		"tier_history": []any{"fabricated"}, "note": "kept",
	}, "# sneaky\n", ""); err != nil {
		t.Fatal(err)
	}

	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	n, _ := ix.Get("sneaky.md")
	if n.Tier != TierDakhil {
		t.Fatalf("tier = %q, want dakhil (model-supplied tier must be stripped)", n.Tier)
	}
	for _, k := range []string{"proposed_tier", "denied_tier", "tier_history"} {
		if _, ok := n.Frontmatter[k]; ok {
			t.Fatalf("frontmatter[%q] survived a create it had no business setting: %v", k, n.Frontmatter[k])
		}
	}
	if n.Frontmatter["note"] != "kept" {
		t.Fatalf("a legitimate frontmatter field was dropped alongside the server-owned ones: %v", n.Frontmatter)
	}

	// now attempt the same thing via write_note, against a note that
	// already carries a real proposal — the model's frontmatter must not
	// be able to erase it either
	if err := ProposeTier(root, "sneaky.md", TierAmil, "a real proposal"); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteNote(root, "sneaky.md", map[string]any{
		"tier": "asil", "denied_tier": "asil", "proposed_tier": "thabit", "note": "still kept",
	}, "# sneaky\n\nedited\n", ""); err != nil {
		t.Fatal(err)
	}

	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	n, _ = ix.Get("sneaky.md")
	if n.Tier != TierDakhil {
		t.Fatalf("tier after write_note = %q, want dakhil unchanged (model can't raise it)", n.Tier)
	}
	p := ProposalFrom(n.Frontmatter)
	if p == nil || p.Tier != TierAmil || p.Reason != "a real proposal" {
		t.Fatalf("proposal = %+v, want the real one to survive a model write that tried to overwrite it", p)
	}
	if _, ok := n.Frontmatter["denied_tier"]; ok {
		t.Fatal("model-supplied denied_tier should never be written at all")
	}
}

func TestSetTierRejectsAsilUnconditionally(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "foundational.md", nil, "# foundational\n", ""); err != nil {
		t.Fatal(err)
	}
	promoteToAsilOnDisk(t, root, "foundational.md")

	if err := SetTier(root, "foundational.md", TierDakhil, "even a lower is refused"); !errors.Is(err, ErrAsilLocked) {
		t.Fatalf("err = %v, want ErrAsilLocked", err)
	}
}

func TestDeleteNoteForbiddenAboveDakhil(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "curated.md", nil, "# curated\n", ""); err != nil {
		t.Fatal(err)
	}
	setTierOnDiskForTest(t, root, "curated.md", TierAmil)

	if err := DeleteNote(root, "curated.md", ""); !errors.Is(err, ErrDeleteForbidden) {
		t.Fatalf("err = %v, want ErrDeleteForbidden", err)
	}
}

// TestAsilWriteLockedAtFilesystem proves the belt-and-braces half of the
// guarantee: even bypassing every application-layer check by calling the
// raw os functions the way WriteNote/DeleteNote do internally, the
// actual file permissions refuse the write — spec/tiers.md's "any write
// to an asil note fails, including via path traversal" requirement.
func TestAsilWriteLockedAtFilesystem(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "foundational.md", nil, "# foundational\n", ""); err != nil {
		t.Fatal(err)
	}
	promoteToAsilOnDisk(t, root, "foundational.md")

	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	n, ok := ix.Get("foundational.md")
	if !ok || n.Tier != TierAsil {
		t.Fatalf("setup failed: note not indexed as asil (%v, ok=%v)", n, ok)
	}

	abs := root + "/foundational.md"
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err == nil {
		f.Close()
		t.Fatal("expected the filesystem itself to refuse opening an asil note for writing")
	}
}

// TestCompactJSONWireFormat proves the wire format is compact JSON, not
// pretty-printed — spec/tiers.md's explicit requirement.
func TestCompactJSONWireFormat(t *testing.T) {
	b, err := json.Marshal(GraphNode{Path: "a.md", Title: "a", Distance: 1, Tier: TierDakhil})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "\n") || strings.Contains(string(b), "  ") {
		t.Fatalf("wire format is not compact: %s", b)
	}
}

func promoteToAsilOnDisk(t *testing.T, root, path string) {
	t.Helper()
	setTierOnDiskForTest(t, root, path, TierAsil)
}

// setTierOnDiskForTest rewrites path's frontmatter tier directly, the
// way a human's own git commit would — no function in this package can
// do this for tiers above what the caller already holds (R1).
func setTierOnDiskForTest(t *testing.T, root, rel string, tier Tier) {
	t.Helper()
	n, abs, err := readCurrentNote(root, rel)
	if err != nil {
		t.Fatal(err)
	}
	fm := map[string]any{"tier": string(tier)}
	for k, v := range n.Frontmatter {
		fm[k] = v
	}
	content, err := serialize(fm, n.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(abs, rel, content); err != nil {
		t.Fatal(err)
	}
	if err := commitPath(root, rel, "curate "+rel+" to "+string(tier)); err != nil {
		t.Fatal(err)
	}
}
