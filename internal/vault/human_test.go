package vault

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func buildIx(t *testing.T, root string) *Index {
	t.Helper()
	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	return ix
}

func TestMarkOpenedPromotesDakhilOnlyAndCommits(t *testing.T) {
	root := gitTempRepo(t)
	CreateNote(root, "n.md", nil, "# n\n", "")
	before := commitCount(t, root)

	promoted, err := MarkOpened(root, "n.md")
	if err != nil || !promoted {
		t.Fatalf("MarkOpened = %v, %v; want promoted", promoted, err)
	}
	if got := commitCount(t, root); got != before+1 {
		t.Fatalf("commits = %d, want exactly one more than %d", got, before)
	}
	if files := lastCommitFiles(t, root); len(files) != 1 || files[0] != "n.md" {
		t.Fatalf("commit touched %v", files)
	}
	n, _ := buildIx(t, root).Get("n.md")
	if n.Tier != TierAmil || strings.TrimPrefix(n.Body, "\n") != "# n\n" {
		t.Fatalf("tier=%s body=%q, want amil with the body untouched", n.Tier, n.Body)
	}

	// already amil: a second open is a no-op with no new commit
	if promoted, _ := MarkOpened(root, "n.md"); promoted {
		t.Fatal("an amil note must not be promoted again")
	}
	if got := commitCount(t, root); got != before+1 {
		t.Fatal("a no-op open must not commit")
	}
}

func TestApproveProposalRaisesClearsAndRecords(t *testing.T) {
	root := gitTempRepo(t)
	CreateNote(root, "n.md", nil, "# n\n", "")
	setTierOnDiskForTest(t, root, "n.md", TierAmil)
	if err := ProposeTier(root, "n.md", TierThabit, "reviewed twice"); err != nil {
		t.Fatal(err)
	}
	if err := ApproveProposal(root, "n.md"); err != nil {
		t.Fatal(err)
	}
	n, _ := buildIx(t, root).Get("n.md")
	if n.Tier != TierThabit {
		t.Fatalf("tier = %s, want thabit", n.Tier)
	}
	if ProposalFrom(n.Frontmatter) != nil {
		t.Fatal("approving must clear the proposal")
	}
	if h := changedHistory(n); h != 1 {
		t.Fatalf("tier_history entries = %d, want 1", h)
	}
	if err := ApproveProposal(root, "n.md"); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("second approve: %v, want ErrNoProposal", err)
	}
}

func changedHistory(n *Note) int { h, _ := n.Frontmatter["tier_history"].([]any); return len(h) }

func TestApproveToAsilWriteLocksImmediately(t *testing.T) {
	root := gitTempRepo(t)
	CreateNote(root, "n.md", nil, "# n\n", "")
	setTierOnDiskForTest(t, root, "n.md", TierThabit)
	ProposeTier(root, "n.md", TierAsil, "foundational")
	if err := ApproveProposal(root, "n.md"); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(root, "n.md"))
	if st.Mode().Perm() != 0o444 {
		t.Fatalf("mode = %v, want 0444 straight after approval, without waiting for an index pass", st.Mode().Perm())
	}
	if _, err := WriteNote(root, "n.md", nil, "x", ""); !errors.Is(err, ErrAsilLocked) {
		t.Fatalf("WriteNote on the new asil note: %v", err)
	}
}

func TestDenyBlocksResubmissionUntilEdited(t *testing.T) {
	root := gitTempRepo(t)
	CreateNote(root, "n.md", nil, "# n\n", "")
	ProposeTier(root, "n.md", TierAmil, "please")
	if err := DenyProposal(root, "n.md", "not yet"); err != nil {
		t.Fatal(err)
	}
	n, _ := buildIx(t, root).Get("n.md")
	d := DenialFrom(n.Frontmatter)
	if d == nil || d.Tier != TierAmil || d.Reason != "not yet" || ProposalFrom(n.Frontmatter) != nil {
		t.Fatalf("denial=%+v proposal=%+v", d, ProposalFrom(n.Frontmatter))
	}
	// the write path honours the denial the human just recorded
	if err := ProposeTier(root, "n.md", TierAmil, "again"); !errors.Is(err, ErrProposalDenied) {
		t.Fatalf("resubmit: %v, want ErrProposalDenied", err)
	}
	// once the note changes (mtime moves well past denied_at), it may return
	time.Sleep(10 * time.Millisecond)
	abs := filepath.Join(root, "n.md")
	future := time.Now().Add(10 * time.Second)
	os.Chtimes(abs, future, future)
	if err := ProposeTier(root, "n.md", TierAmil, "again, edited"); err != nil {
		t.Fatalf("after an edit: %v", err)
	}
}

func TestDeferClearsWithoutDenial(t *testing.T) {
	root := gitTempRepo(t)
	CreateNote(root, "n.md", nil, "# n\n", "")
	ProposeTier(root, "n.md", TierAmil, "maybe")
	if err := DeferProposal(root, "n.md"); err != nil {
		t.Fatal(err)
	}
	n, _ := buildIx(t, root).Get("n.md")
	if ProposalFrom(n.Frontmatter) != nil || DenialFrom(n.Frontmatter) != nil {
		t.Fatal("defer must clear the proposal and write no denial")
	}
	if err := ProposeTier(root, "n.md", TierAmil, "ok now"); err != nil {
		t.Fatalf("propose after defer: %v", err)
	}
}

func TestHumanActionsRefuseAsil(t *testing.T) {
	root := gitTempRepo(t)
	CreateNote(root, "n.md", nil, "# n\n", "")
	setTierOnDiskForTest(t, root, "n.md", TierAsil)
	if err := DenyProposal(root, "n.md", ""); !errors.Is(err, ErrAsilLocked) {
		t.Fatalf("deny on asil: %v", err)
	}
	if promoted, err := MarkOpened(root, "n.md"); promoted || err != nil {
		t.Fatalf("MarkOpened on asil = %v, %v", promoted, err)
	}
}

// TestOnlyTheRendererCanRaiseTiers is spec/tiers.md's "approving from
// Al-Mina is the only code path that raises a tier", enforced by reading
// the source: outside this package, the human-only functions may be
// referenced from internal/render alone — never from internal/mcp or
// anywhere else — so no model-reachable path can promote a note.
func TestOnlyTheRendererCanRaiseTiers(t *testing.T) {
	re := regexp.MustCompile(`\b(MarkOpened|ApproveProposal|DenyProposal|DeferProposal)\b`)
	repo := filepath.Join("..", "..")
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		slashed := filepath.ToSlash(p)
		if strings.Contains(slashed, "internal/vault/") || strings.Contains(slashed, "internal/render/") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if re.Match(b) {
			t.Errorf("%s references a human-only tier action; only internal/render may", slashed)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMetadataRewritesDoNotGrowTheBody guards the blank-line accumulation
// bug: every frontmatter-only rewrite must leave the body byte-stable.
func TestMetadataRewritesDoNotGrowTheBody(t *testing.T) {
	root := gitTempRepo(t)
	CreateNote(root, "n.md", map[string]any{"k": "v"}, "# n\n\ntext\n", "")
	start, _ := os.ReadFile(filepath.Join(root, "n.md"))
	body := func() string {
		b, _ := os.ReadFile(filepath.Join(root, "n.md"))
		return string(b[strings.Index(string(b), "\n---\n")+5:])
	}
	want := body()
	ProposeTier(root, "n.md", TierAmil, "a")
	DeferProposal(root, "n.md")
	ProposeTier(root, "n.md", TierAmil, "b")
	DenyProposal(root, "n.md", "no")
	MarkOpened(root, "n.md")
	if got := body(); got != want {
		t.Fatalf("body drifted across metadata rewrites:\nstart %q\nnow   %q (file started as %q)", want, got, start)
	}
}
