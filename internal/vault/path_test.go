package vault

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSafeRequestPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		p    string
		want string
		err  error
	}{
		{"clean path", "a/b/c.md", "a/b/c.md", nil},
		{"existing note", "note.md", "note.md", nil},
		{"missing note passes", "ghost.md", "ghost.md", nil},
		{"redundant segments cleaned", "a//b/./c.md", "a/b/c.md", nil},
		{"traversal", "../../etc/passwd", "", ErrPathUnsafe},
		{"interior traversal escaping", "a/../../etc/passwd", "", ErrPathUnsafe},
		{"absolute", "/etc/passwd", "", ErrPathUnsafe},
		{"backslash", `..\..\etc\passwd`, "", ErrPathUnsafe},
		{"empty", "", "", ErrPathUnsafe},
		{"dot", ".", "", ErrPathUnsafe},
		{"null byte", "a\x00.md", "", ErrPathUnsafe},
		{"hidden file", ".env", "", ErrPathHidden},
		{"hidden dir", ".git/config", "", ErrPathHidden},
		{"hidden mid-path", "a/.git/config", "", ErrPathHidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SafeRequestPath(root, tt.p)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSafeRequestPathSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("# secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a symlinked file pointing outside the vault
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "leak.md")); err != nil {
		t.Fatal(err)
	}
	// a symlinked directory pointing outside the vault
	if err := os.Symlink(outside, filepath.Join(root, "leakdir")); err != nil {
		t.Fatal(err)
	}

	if _, err := SafeRequestPath(root, "leak.md"); !errors.Is(err, ErrPathUnsafe) {
		t.Fatalf("symlinked file escape: err = %v, want ErrPathUnsafe", err)
	}
	if _, err := SafeRequestPath(root, "leakdir/secret.md"); !errors.Is(err, ErrPathUnsafe) {
		t.Fatalf("symlinked dir escape: err = %v, want ErrPathUnsafe", err)
	}

	// a symlink pointing inside the vault is fine
	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("# real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real.md"), filepath.Join(root, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeRequestPath(root, "alias.md"); err != nil {
		t.Fatalf("internal symlink should pass: %v", err)
	}
}

func TestIndexSkipsSymlinkedNotes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("# secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "leak.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.md"), []byte("# ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := NewIndex(root, testLogger())
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Get("leak.md"); ok {
		t.Fatal("symlinked note was indexed")
	}
	if _, ok := ix.Get("ok.md"); !ok {
		t.Fatal("regular note missing")
	}
}
