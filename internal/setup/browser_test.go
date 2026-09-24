package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture: root/{vault/{.git,a.md,sub/}, plain/, .hidden/, link->outside}
// and a sibling directory outside root.
func fixture(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{"root/vault/.git", "root/vault/sub", "root/plain", "root/.hidden", "outside/secret"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "vault", "a.md"), []byte("# a"), 0o644)
	os.WriteFile(filepath.Join(root, "vault", "sub", "b.md"), []byte("# b"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	return root, outside
}

func TestResolveConfinesToRoot(t *testing.T) {
	root, outside := fixture(t)
	b, err := NewBrowser(root)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"outside sibling":     outside,
		"dotdot escape":       filepath.Join(root, "..", "outside"),
		"dotdot via subdir":   filepath.Join(root, "plain", "..", "..", "outside"),
		"symlink escape":      filepath.Join(root, "link"),
		"symlink then inside": filepath.Join(root, "link", "secret"),
		"relative":            "vault",
		"root's parent":       filepath.Dir(root),
		"filesystem root":     "/",
		"hidden dir":          filepath.Join(root, ".hidden"),
		"dot-git inside":      filepath.Join(root, "vault", ".git"),
		"nul byte":            root + "\x00/vault",
		"missing":             filepath.Join(root, "nope"),
		"a file":              filepath.Join(root, "vault", "a.md"),
	}
	for name, dir := range bad {
		if got, err := b.Resolve(dir); err == nil {
			t.Errorf("%s: Resolve(%q) = %q, want an error", name, dir, got)
		}
	}
	for name, dir := range map[string]string{
		"empty is root": "",
		"root":          root,
		"vault":         filepath.Join(root, "vault"),
		"nested":        filepath.Join(root, "vault", "sub"),
		"dotdot inside": filepath.Join(root, "plain", "..", "vault"),
	} {
		if _, err := b.Resolve(dir); err != nil {
			t.Errorf("%s: Resolve(%q): %v", name, dir, err)
		}
	}
}

func TestListShowsOnlyVisibleRealDirs(t *testing.T) {
	root, _ := fixture(t)
	b, _ := NewBrowser(root)
	l, err := b.List(b.Root())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Subdirs {
		names = append(names, e.Name)
		if e.Name == "vault" && !e.IsGit {
			t.Error("vault should be flagged as a git repository")
		}
	}
	if len(names) != 2 || names[0] != "plain" || names[1] != "vault" {
		t.Fatalf("subdirs = %v, want [plain vault] (no hidden dir, no symlink)", names)
	}
}

func TestListCountsNotesAndCrumbs(t *testing.T) {
	root, _ := fixture(t)
	b, _ := NewBrowser(root)
	dir, _ := b.Resolve(filepath.Join(root, "vault"))
	l, _ := b.List(dir)
	if l.Notes != 2 || !l.IsGit {
		t.Fatalf("Notes=%d IsGit=%v, want 2/true", l.Notes, l.IsGit)
	}
	if len(l.Crumbs) != 2 || l.Crumbs[1].Name != "vault" {
		t.Fatalf("crumbs = %+v", l.Crumbs)
	}
	sub, _ := b.Resolve(filepath.Join(root, "vault", "sub"))
	if l, _ := b.List(sub); !l.IsGit {
		t.Error("a directory inside a git repo should report IsGit")
	}
}
