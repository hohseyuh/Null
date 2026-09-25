package vault

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
)

// gitTempRepo returns a fresh temp dir initialized as a git repo, with
// an initial empty commit so "HEAD" always resolves — every write test
// needs this now that every write commits.
func gitTempRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "null-test"},
		{"config", "user.email", "null-test@localhost"},
		{"commit", "--allow-empty", "-q", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(cmd.Environ(), gitIdentityEnv()...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return root
}

// commitCount returns the number of commits reachable from HEAD.
func commitCount(t *testing.T, root string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	n := 0
	for _, c := range strings.TrimSpace(string(out)) {
		n = n*10 + int(c-'0')
	}
	return n
}

// lastCommitFiles returns the files changed in HEAD.
func lastCommitFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "show", "--name-only", "--pretty=format:", "HEAD").Output()
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

func lastCommitMessage(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "log", "-1", "--pretty=%B").Output()
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	return string(out)
}

// gitOutput runs a git command in root and returns its stdout, failing
// the test on error.
func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func TestEnsureGitRepo(t *testing.T) {
	if err := EnsureGitRepo(gitTempRepo(t)); err != nil {
		t.Fatalf("a real git repo should pass: %v", err)
	}
	if err := EnsureGitRepo(t.TempDir()); err == nil {
		t.Fatal("a plain directory should fail EnsureGitRepo")
	}
}

func TestCreateNoteCommitsExactlyOneFile(t *testing.T) {
	root := gitTempRepo(t)
	before := commitCount(t, root)

	if err := CreateNote(root, "draft.md", map[string]any{"status": "draft"}, "# draft\n\nfirst version\n", "why: testing"); err != nil {
		t.Fatalf("CreateNote: %v", err)
	}

	if got := commitCount(t, root); got != before+1 {
		t.Fatalf("commit count = %d, want %d (exactly one new commit)", got, before+1)
	}
	if files := lastCommitFiles(t, root); len(files) != 1 || files[0] != "draft.md" {
		t.Fatalf("commit touched %v, want exactly [draft.md]", files)
	}
	msg := lastCommitMessage(t, root)
	if !strings.HasPrefix(msg, "Add draft.md") || !strings.Contains(msg, "why: testing") || !strings.Contains(msg, "Source: nullmcp create_note") {
		t.Fatalf("commit message = %q", msg)
	}

	// second create at the same path must fail, never silently overwrite,
	// and must not produce a new commit
	beforeSecond := commitCount(t, root)
	if err := CreateNote(root, "draft.md", nil, "clobber", ""); !errors.Is(err, ErrNoteExists) {
		t.Fatalf("second CreateNote: err = %v, want ErrNoteExists", err)
	}
	if got := commitCount(t, root); got != beforeSecond {
		t.Fatalf("a rejected create must not commit: count = %d, want %d", got, beforeSecond)
	}

	// what was written parses back correctly
	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	n, ok := ix.Get("draft.md")
	if !ok {
		t.Fatal("created note not found by the index")
	}
	if n.Frontmatter["status"] != "draft" {
		t.Fatalf("frontmatter = %v", n.Frontmatter)
	}
	if n.Body != "\n# draft\n\nfirst version\n" { // blank line after the fence, matching the vault's own note convention
		t.Fatalf("body = %q", n.Body)
	}
}

func TestWriteNoteCommitsExactlyOneFile(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "draft.md", nil, "# draft\n\nfirst version\n", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteNote(root, "nope.md", nil, "x", ""); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("WriteNote on missing: err = %v, want ErrNoteNotFound", err)
	}

	before := commitCount(t, root)
	if _, err := WriteNote(root, "draft.md", map[string]any{"status": "revised"}, "# draft\n\nsecond version\n", ""); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
	if got := commitCount(t, root); got != before+1 {
		t.Fatalf("commit count = %d, want %d", got, before+1)
	}
	if files := lastCommitFiles(t, root); len(files) != 1 || files[0] != "draft.md" {
		t.Fatalf("commit touched %v, want exactly [draft.md]", files)
	}
	if !strings.HasPrefix(lastCommitMessage(t, root), "Update draft.md") {
		t.Fatalf("commit message = %q", lastCommitMessage(t, root))
	}

	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	n, _ := ix.Get("draft.md")
	if n.Frontmatter["status"] != "revised" || n.Body != "\n# draft\n\nsecond version\n" {
		t.Fatalf("after write: frontmatter=%v body=%q", n.Frontmatter, n.Body)
	}
}

func TestConsecutiveWriteNoteCallsCollapseIntoOneCommit(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "draft.md", nil, "# draft\n\nv1\n", ""); err != nil {
		t.Fatal(err)
	}
	before := commitCount(t, root)

	// ten edits in a row to the same note
	for i := 2; i <= 10; i++ {
		body := fmt.Sprintf("# draft\n\nv%d\n", i)
		if _, err := WriteNote(root, "draft.md", nil, body, fmt.Sprintf("revision %d", i)); err != nil {
			t.Fatalf("WriteNote v%d: %v", i, err)
		}
	}

	// the first WriteNote (v1 -> v2) starts a fresh "Update" commit;
	// every one after that (v2->v3 ... v9->v10) amends it in place — so
	// nine WriteNote calls produce exactly one new commit, not nine.
	if got := commitCount(t, root); got != before+1 {
		t.Fatalf("commit count = %d, want %d (nine edits should collapse into one commit)", got, before+1)
	}
	if files := lastCommitFiles(t, root); len(files) != 1 || files[0] != "draft.md" {
		t.Fatalf("commit touched %v, want exactly [draft.md]", files)
	}

	// the final state is v10, and only the LATEST reason survives —
	// stale intermediate reasons aren't kept piling up in the message
	msg := lastCommitMessage(t, root)
	if !strings.Contains(msg, "revision 10") {
		t.Fatalf("commit message should carry the latest reason: %q", msg)
	}
	if strings.Contains(msg, "revision 9") || strings.Contains(msg, "revision 2") {
		t.Fatalf("commit message should not accumulate stale reasons: %q", msg)
	}

	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	n, _ := ix.Get("draft.md")
	if n.Body != "# draft\n\nv10\n" { // no frontmatter this time, so no fence, so no leading blank line
		t.Fatalf("final body = %q, want v10", n.Body)
	}

	// the amended commit's PARENT is still the original "Add" commit —
	// amending replaced the tip, it didn't insert extra history beneath it
	parentSubject := strings.TrimSpace(gitOutput(t, root, "log", "-1", "--skip=1", "--pretty=%s"))
	if parentSubject != "Add draft.md" {
		t.Fatalf("parent of the collapsed commit = %q, want %q", parentSubject, "Add draft.md")
	}
}

func TestWriteNoteDoesNotAmendAcrossADifferentCommit(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "a.md", nil, "# a\n\nv1\n", ""); err != nil {
		t.Fatal(err)
	}
	if err := CreateNote(root, "b.md", nil, "# b\n", ""); err != nil {
		t.Fatal(err)
	}
	before := commitCount(t, root)

	// edit a.md, then something else happens to b.md, then edit a.md
	// again — the chain is broken, so this must NOT amend across b.md's
	// commit and silently drop it from history
	if _, err := WriteNote(root, "a.md", nil, "# a\n\nv2\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteNote(root, "b.md", nil, "# b\n\nedited\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteNote(root, "a.md", nil, "# a\n\nv3\n", ""); err != nil {
		t.Fatal(err)
	}

	if got := commitCount(t, root); got != before+3 {
		t.Fatalf("commit count = %d, want %d (three genuinely separate edits)", got, before+3)
	}
	// b.md's commit must still be there, untouched, in the middle
	subjects := []string{
		strings.TrimSpace(gitOutput(t, root, "log", "-1", "--skip=2", "--pretty=%s")),
		strings.TrimSpace(gitOutput(t, root, "log", "-1", "--skip=1", "--pretty=%s")),
		strings.TrimSpace(gitOutput(t, root, "log", "-1", "--skip=0", "--pretty=%s")),
	}
	want := []string{"Update a.md", "Update b.md", "Update a.md"}
	if subjects[0] != want[0] || subjects[1] != want[1] || subjects[2] != want[2] {
		t.Fatalf("commit sequence = %v, want %v", subjects, want)
	}
}

func TestCreateAndDeleteNeverAmend(t *testing.T) {
	root := gitTempRepo(t)

	// two creates in a row (different paths) must never collapse —
	// amending is write_note-only, by design
	if err := CreateNote(root, "one.md", nil, "# one\n", ""); err != nil {
		t.Fatal(err)
	}
	beforeSecondCreate := commitCount(t, root)
	if err := CreateNote(root, "two.md", nil, "# two\n", ""); err != nil {
		t.Fatal(err)
	}
	if got := commitCount(t, root); got != beforeSecondCreate+1 {
		t.Fatalf("second CreateNote must be its own commit: count = %d, want %d", got, beforeSecondCreate+1)
	}

	// write then delete the same path: delete must be its own commit,
	// never amended into the prior update
	if _, err := WriteNote(root, "one.md", nil, "# one\n\nrevised\n", ""); err != nil {
		t.Fatal(err)
	}
	beforeDelete := commitCount(t, root)
	if err := DeleteNote(root, "one.md", ""); err != nil {
		t.Fatal(err)
	}
	if got := commitCount(t, root); got != beforeDelete+1 {
		t.Fatalf("DeleteNote must be its own commit: count = %d, want %d", got, beforeDelete+1)
	}
	if msg := lastCommitMessage(t, root); !strings.HasPrefix(msg, "Delete one.md") {
		t.Fatalf("commit message = %q", msg)
	}
}

func TestDeleteNoteCommitsExactlyOneFile(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "draft.md", nil, "# draft\n", ""); err != nil {
		t.Fatal(err)
	}

	if err := DeleteNote(root, "nope.md", ""); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("DeleteNote on missing: err = %v, want ErrNoteNotFound", err)
	}

	before := commitCount(t, root)
	if err := DeleteNote(root, "draft.md", "cleaning up"); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if got := commitCount(t, root); got != before+1 {
		t.Fatalf("commit count = %d, want %d", got, before+1)
	}
	if files := lastCommitFiles(t, root); len(files) != 1 || files[0] != "draft.md" {
		t.Fatalf("commit touched %v, want exactly [draft.md]", files)
	}
	msg := lastCommitMessage(t, root)
	if !strings.HasPrefix(msg, "Delete draft.md") || !strings.Contains(msg, "cleaning up") {
		t.Fatalf("commit message = %q", msg)
	}

	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Get("draft.md"); ok {
		t.Fatal("deleted note still indexed")
	}

	// the deletion is a real, isolated commit — reverting it alone
	// restores exactly this file, proving the "one commit = one clean
	// undo" property the whole design rests on
	if out, err := exec.Command("git", "-C", root, "revert", "--no-edit", "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("git revert of the delete commit: %v: %s", err, out)
	}
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Get("draft.md"); !ok {
		t.Fatal("reverting the delete commit should restore the file")
	}
}

func TestCreateNoteSubfolder(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "topic/sub/note.md", nil, "# note\n", ""); err != nil {
		t.Fatalf("CreateNote with subfolders: %v", err)
	}
	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Get("topic/sub/note.md"); !ok {
		t.Fatal("note in a new subfolder not indexed")
	}
}

func TestCreateNoteNoFrontmatter(t *testing.T) {
	root := gitTempRepo(t)
	if err := CreateNote(root, "bare.md", nil, "# bare\n\nno frontmatter\n", ""); err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	n, _ := ix.Get("bare.md")
	if len(n.Frontmatter) != 0 {
		t.Fatalf("expected no frontmatter, got %v", n.Frontmatter)
	}
	if n.Body != "# bare\n\nno frontmatter\n" {
		t.Fatalf("body = %q", n.Body)
	}
}

func TestConcurrentWritesEachGetTheirOwnCommit(t *testing.T) {
	root := gitTempRepo(t)
	before := commitCount(t, root)

	const n = 10
	errs := make(chan error, n)
	for i := range n {
		go func(i int) {
			errs <- CreateNote(root, pathFor(i), nil, "# note\n", "")
		}(i)
	}
	for range n {
		if err := <-errs; err != nil {
			t.Errorf("CreateNote: %v", err)
		}
	}

	if got := commitCount(t, root); got != before+n {
		t.Fatalf("commit count = %d, want %d — concurrent writes must never share a commit", got, before+n)
	}
}

func pathFor(i int) string {
	return "concurrent-" + string(rune('a'+i)) + ".md"
}
