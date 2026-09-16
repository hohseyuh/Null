package vault

import (
	"errors"
	"log/slog"
	"testing"
)

func TestCreateAndWriteNote(t *testing.T) {
	root := t.TempDir()

	// create
	if err := CreateNote(root, "draft.md", map[string]any{"status": "draft"}, "# draft\n\nfirst version\n"); err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	// second create at the same path must fail, never silently overwrite
	if err := CreateNote(root, "draft.md", nil, "clobber"); !errors.Is(err, ErrNoteExists) {
		t.Fatalf("second CreateNote: err = %v, want ErrNoteExists", err)
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
	// matches the vault's own convention: a blank line separates the
	// closing frontmatter fence from the body (see any fixture note).
	if n.Body != "\n# draft\n\nfirst version\n" {
		t.Fatalf("body = %q", n.Body)
	}

	// write requires the note to already exist
	if err := WriteNote(root, "nope.md", nil, "x"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("WriteNote on missing: err = %v, want ErrNoteNotFound", err)
	}

	// write overwrites wholesale
	if err := WriteNote(root, "draft.md", map[string]any{"status": "revised"}, "# draft\n\nsecond version\n"); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	n, _ = ix.Get("draft.md")
	if n.Frontmatter["status"] != "revised" || n.Body != "\n# draft\n\nsecond version\n" {
		t.Fatalf("after write: frontmatter=%v body=%q", n.Frontmatter, n.Body)
	}
}

func TestCreateNoteSubfolder(t *testing.T) {
	root := t.TempDir()
	if err := CreateNote(root, "topic/sub/note.md", nil, "# note\n"); err != nil {
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
	root := t.TempDir()
	if err := CreateNote(root, "bare.md", nil, "# bare\n\nno frontmatter\n"); err != nil {
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
