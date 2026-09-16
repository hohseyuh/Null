package vault

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Edge is one directed, resolved wikilink between two notes. Context is
// the full line the link appeared on in the From note.
type Edge struct {
	From    string
	To      string
	Context string
	Line    int
}

// Index is the in-memory view of the vault: path → note, plus the reverse
// edge map for backlinks. It is rebuilt on boot and patched by the watcher.
// Notes handed out by Get/All are treated as immutable — updates replace
// the pointer, never mutate through it — so callers may hold them after
// releasing no locks of their own.
type Index struct {
	root string
	log  *slog.Logger

	// Source is stamped onto every Note this index parses. Defaults to
	// SourceVault; set directly (Index.Source = vault.SourceInbox)
	// before Build for a secondary root such as the inbox.
	Source string

	mu        sync.RWMutex
	notes     map[string]*Note
	backlinks map[string][]Edge // key: To path
	resolver  *Resolver         // rebuilt with the backlink map

	reparses atomic.Int64
}

// Reader is the read surface both a plain Index and a Combined (a merged
// vault+inbox view) satisfy. internal/mcp depends on this instead of
// *Index directly so it can run against either.
type Reader interface {
	Get(path string) (*Note, bool)
	All() []*Note
	Backlinks(path string) []Edge
	Outlinks(path string) []Edge
	Graph(root string, depth int, direction string) GraphResult
	Resolve(target string) (string, bool)
	Len() int
}

// NewIndex creates an empty index over the vault rooted at root, sourced
// as SourceVault. Call Build before serving. log must be non-nil.
func NewIndex(root string, log *slog.Logger) *Index {
	return &Index{
		root:      root,
		log:       log,
		Source:    SourceVault,
		notes:     map[string]*Note{},
		backlinks: map[string][]Edge{},
	}
}

// isHidden reports whether any segment of the slash-separated relative
// path starts with a dot. Dotfiles and dot-directories are invisible to
// the whole service, .git/ above all.
func isHidden(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// readNoteFile reads one file strictly O_RDONLY — the non-negotiable
// enforcement that this service can never write to the vault — and returns
// its bytes and mtime. Symlinks and other irregular files are refused, so
// a link planted inside the vault cannot pull outside content into the
// index. abs must already be a validated path under root.
func readNoteFile(abs string) ([]byte, time.Time, error) {
	st, err := os.Lstat(abs)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !st.Mode().IsRegular() {
		return nil, time.Time{}, fmt.Errorf("%s: not a regular file", abs)
	}
	f, err := os.OpenFile(abs, os.O_RDONLY, 0)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	st, err = f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, time.Time{}, err
	}
	return b, st.ModTime(), nil
}

// Build walks the vault, parses every visible .md file, and publishes the
// index. It assumes the root exists and logs boot timing; a single
// unreadable file is logged and skipped, never fatal.
func (ix *Index) Build() error {
	start := time.Now()
	parsed := map[string]*Note{}

	err := filepath.WalkDir(ix.root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(ix.root, abs)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if isHidden(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(rel, ".md") {
			return nil
		}
		raw, mtime, rerr := readNoteFile(abs)
		if rerr != nil {
			ix.log.Warn("skipping unreadable note", "path", rel, "err", rerr)
			return nil
		}
		n := Parse(rel, raw, mtime, ix.log)
		n.Source = ix.Source
		parsed[rel] = n
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk vault: %w", err)
	}

	ix.mu.Lock()
	ix.notes = parsed
	ix.rebuildResolutionLocked()
	ix.mu.Unlock()

	ix.log.Info("index built",
		"notes", len(parsed),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

// rebuildResolutionLocked re-resolves every note's outlinks against the
// current path set and rebuilds the backlink map. Notes whose links it
// touches are replaced by shallow copies, preserving the immutability of
// pointers already handed out. Caller must hold the write lock. Cheap:
// map lookups only, no disk, no reparse.
func (ix *Index) rebuildResolutionLocked() {
	paths := make([]string, 0, len(ix.notes))
	for p := range ix.notes {
		paths = append(paths, p)
	}
	r := NewResolver(paths)
	ix.resolver = r

	backlinks := make(map[string][]Edge)
	for p, n := range ix.notes {
		cp := *n
		cp.Outlinks = append([]Link(nil), n.Outlinks...)
		cp.ResolveLinks(r)
		ix.notes[p] = &cp
		for _, l := range cp.Outlinks {
			if !l.Resolved || l.Path == p {
				continue
			}
			backlinks[l.Path] = append(backlinks[l.Path], Edge{
				From: p, To: l.Path, Context: l.Context, Line: l.Line,
			})
		}
	}
	for _, edges := range backlinks {
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].From != edges[j].From {
				return edges[i].From < edges[j].From
			}
			return edges[i].Line < edges[j].Line
		})
	}
	ix.backlinks = backlinks
}

// pathsUnder returns indexed note paths with the given slash-terminated
// prefix. Used by the watcher to expand a removed directory into the notes
// it contained.
func (ix *Index) pathsUnder(prefix string) []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []string
	for p := range ix.notes {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// applyBatch reparses the given paths (deleting the ones that no longer
// exist) and re-resolves the link graph once for the whole batch. This is
// the debounced entry point the watcher calls, sized for a git pull that
// lands fifty files at once.
func (ix *Index) applyBatch(rels []string) {
	type parsedNote struct {
		rel string
		n   *Note // nil means delete
	}
	batch := make([]parsedNote, 0, len(rels))
	for _, rel := range rels {
		abs := filepath.Join(ix.root, filepath.FromSlash(rel))
		raw, mtime, err := readNoteFile(abs)
		if err != nil {
			if os.IsNotExist(err) {
				batch = append(batch, parsedNote{rel, nil})
			} else {
				ix.log.Warn("skipping unreadable note", "path", rel, "err", err)
			}
			continue
		}
		ix.reparses.Add(1)
		n := Parse(rel, raw, mtime, ix.log)
		n.Source = ix.Source
		batch = append(batch, parsedNote{rel, n})
	}

	ix.mu.Lock()
	for _, pn := range batch {
		if pn.n == nil {
			delete(ix.notes, pn.rel)
		} else {
			ix.notes[pn.rel] = pn.n
		}
	}
	ix.rebuildResolutionLocked()
	total := len(ix.notes)
	ix.mu.Unlock()

	ix.log.Info("index patched", "changed", len(batch), "notes", total)
}

// Refresh immediately reparses one file and re-resolves the link graph,
// synchronously — the same work the watcher's debounced applyBatch does,
// exposed for a caller that just wrote rel itself and wants the index to
// reflect it before its next read, rather than racing the watcher's
// debounce window. A no-op wait is exactly what CreateNote/WriteNote
// callers would otherwise need to build themselves.
func (ix *Index) Refresh(rel string) {
	ix.applyBatch([]string{rel})
}

// Get returns the note at the exact vault-relative path. The returned
// note must not be mutated.
func (ix *Index) Get(rel string) (*Note, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n, ok := ix.notes[rel]
	return n, ok
}

// All returns every indexed note, sorted by path. The slice is fresh; the
// notes it points at must not be mutated.
func (ix *Index) All() []*Note {
	ix.mu.RLock()
	out := make([]*Note, 0, len(ix.notes))
	for _, n := range ix.notes {
		out = append(out, n)
	}
	ix.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Backlinks returns the edges pointing at rel, sorted by (from, line).
func (ix *Index) Backlinks(rel string) []Edge {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return append([]Edge(nil), ix.backlinks[rel]...)
}

// Outlinks returns the resolved outgoing edges of rel, excluding
// unresolved links and self-links — the mirror of Backlinks for /graph.
func (ix *Index) Outlinks(rel string) []Edge {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n, ok := ix.notes[rel]
	if !ok {
		return nil
	}
	var out []Edge
	for _, l := range n.Outlinks {
		if !l.Resolved || l.Path == rel {
			continue
		}
		out = append(out, Edge{From: rel, To: l.Path, Context: l.Context, Line: l.Line})
	}
	return out
}

// Resolve maps a wikilink target to an indexed path using the resolver
// built at the last index rebuild. The renderer uses this to rewrite
// wikilinks without maintaining a second parse.
func (ix *Index) Resolve(target string) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ix.resolver == nil {
		return "", false
	}
	return ix.resolver.Resolve(target)
}

// Len returns the number of indexed notes.
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.notes)
}

// Reparses returns how many single-file parses the index has performed
// since boot, excluding Build. Exists so tests can prove the watcher's
// debounce holds under a burst.
func (ix *Index) Reparses() int64 {
	return ix.reparses.Load()
}
