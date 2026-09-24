// Package setup is the first-run page: a browser UI for choosing which
// directory is the vault, instead of editing environment variables. The
// dangerous part is that it lets a web request name a filesystem path, so
// everything it can touch is confined to one configured root, with
// symlinks resolved before the containment check — the same discipline as
// vault.SafeRequestPath, applied to directories.
package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrOutsideRoot is returned for any path that is not the browse root or
// a visible directory beneath it, after symlinks are resolved.
var ErrOutsideRoot = errors.New("path is outside the browsable root")

// Browser lists and validates directories under one root.
type Browser struct {
	root string // absolute, symlink-resolved
}

// NewBrowser returns a Browser confined to root, which must exist and be
// a directory. The root itself is symlink-resolved once here so every
// later containment check compares like with like.
func NewBrowser(root string) (*Browser, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("browse root: %w", err)
	}
	st, err := os.Stat(real)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("browse root %s is not a directory", root)
	}
	return &Browser{root: real}, nil
}

// Root returns the resolved browse root.
func (b *Browser) Root() string { return b.root }

// Resolve turns a requested directory into a canonical absolute path, or
// an error. Empty means the root. Relative paths, NUL bytes, missing or
// non-directory targets, any dot-segment below the root (.git and friends
// are never a vault's business), and anything whose real path escapes the
// root — including via a symlink — are all refused.
func (b *Browser) Resolve(dir string) (string, error) {
	if dir == "" {
		return b.root, nil
	}
	if strings.ContainsRune(dir, 0) || !filepath.IsAbs(dir) {
		return "", ErrOutsideRoot
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return "", ErrOutsideRoot // absent and forbidden look the same to the caller
	}
	rel, err := filepath.Rel(b.root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrOutsideRoot
	}
	if rel != "." {
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			if strings.HasPrefix(seg, ".") {
				return "", ErrOutsideRoot
			}
		}
	}
	st, err := os.Stat(real)
	if err != nil || !st.IsDir() {
		return "", ErrOutsideRoot
	}
	return real, nil
}

// Entry is one subdirectory in a listing.
type Entry struct {
	Name  string
	Path  string
	IsGit bool
}

// Crumb is one step of the breadcrumb trail back to the root.
type Crumb struct {
	Name string
	Path string
}

// Listing is what the page shows for one directory.
type Listing struct {
	Dir         string
	Crumbs      []Crumb
	Subdirs     []Entry
	IsGit       bool // Dir itself is a git repository root or inside one
	Notes       int  // .md files found under Dir (capped; see NotesCapped)
	NotesCapped bool
}

const noteScanCap = 5000

// List describes dir (already validated by Resolve): visible immediate
// subdirectories, breadcrumbs, whether dir is under git, and roughly how
// many markdown notes it holds. Symlinked subdirectories are skipped —
// they could point anywhere.
func (b *Browser) List(dir string) (Listing, error) {
	l := Listing{Dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return l, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // IsDir is false for a symlink, which is exactly what we want
		}
		p := filepath.Join(dir, e.Name())
		_, gitErr := os.Stat(filepath.Join(p, ".git"))
		l.Subdirs = append(l.Subdirs, Entry{Name: e.Name(), Path: p, IsGit: gitErr == nil})
	}
	sort.Slice(l.Subdirs, func(i, j int) bool { return l.Subdirs[i].Name < l.Subdirs[j].Name })

	rel, _ := filepath.Rel(b.root, dir)
	l.Crumbs = append(l.Crumbs, Crumb{Name: b.root, Path: b.root})
	if rel != "." {
		cur := b.root
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, seg)
			l.Crumbs = append(l.Crumbs, Crumb{Name: seg, Path: cur})
		}
	}

	l.IsGit = insideGit(dir, b.root)
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			if l.Notes++; l.Notes >= noteScanCap {
				l.NotesCapped = true
				return fs.SkipAll
			}
		}
		return nil
	})
	return l, nil
}

// insideGit reports whether dir or an ancestor up to root holds a .git.
func insideGit(dir, root string) bool {
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return true
		}
		if cur == root || cur == filepath.Dir(cur) {
			return false
		}
	}
}
