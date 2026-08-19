package vault

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher keeps the index in sync with the vault on disk. It watches every
// visible directory recursively, coalesces event bursts with a debounce
// window, and hands the settled set of changed paths to the index in one
// batch — so a git pull that touches fifty notes costs one resolution
// rebuild, not fifty.
type Watcher struct {
	ix       *Index
	fs       *fsnotify.Watcher
	debounce time.Duration
}

// NewWatcher creates a watcher over the index's vault root and registers
// watches on all existing visible directories. It assumes the index has
// already been built. Close it by cancelling the context passed to Run.
func NewWatcher(ix *Index, debounce time.Duration) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify: %w", err)
	}
	w := &Watcher{ix: ix, fs: fsw, debounce: debounce}
	if err := w.watchTree(ix.root); err != nil {
		fsw.Close()
		return nil, err
	}
	return w, nil
}

// watchTree adds watches on dir and every visible directory beneath it.
func (w *Watcher) watchTree(dir string) error {
	return filepath.WalkDir(dir, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(w.ix.root, abs)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel != "." && isHidden(rel) {
			return filepath.SkipDir
		}
		if err := w.fs.Add(abs); err != nil {
			return fmt.Errorf("watch %s: %w", abs, err)
		}
		return nil
	})
}

// Run processes events until ctx is cancelled. Events accumulate into a
// pending set; the set is flushed to the index w.debounce after the last
// event — editors and git both write in bursts, and the burst must settle
// as one batch.
func (w *Watcher) Run(ctx context.Context) {
	defer w.fs.Close()

	pending := map[string]struct{}{}
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-w.fs.Events:
			if !ok {
				return
			}
			if w.observe(ev, pending) {
				timer.Reset(w.debounce)
			}

		case err, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			w.ix.log.Warn("watcher error", "err", err)

		case <-timer.C:
			if len(pending) == 0 {
				continue
			}
			batch := make([]string, 0, len(pending))
			for rel := range pending {
				batch = append(batch, rel)
			}
			clear(pending)
			w.ix.applyBatch(batch)
		}
	}
}

// observe records one fsnotify event into the pending set and reports
// whether it was relevant. New directories get watched immediately —
// a git pull can create a directory and fill it before the debounce
// fires — and any .md files already inside them are queued.
func (w *Watcher) observe(ev fsnotify.Event, pending map[string]struct{}) bool {
	rel, err := filepath.Rel(w.ix.root, ev.Name)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || isHidden(rel) {
		return false
	}

	if ev.Op.Has(fsnotify.Create) {
		if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
			if err := w.watchTree(ev.Name); err != nil {
				w.ix.log.Warn("watch new dir", "path", rel, "err", err)
			}
			// files may have landed before the watch existed
			filepath.WalkDir(ev.Name, func(abs string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(abs, ".md") {
					return nil
				}
				if r, err := filepath.Rel(w.ix.root, abs); err == nil {
					r = filepath.ToSlash(r)
					if !isHidden(r) {
						pending[r] = struct{}{}
					}
				}
				return nil
			})
			return true
		}
	}

	if ev.Op.Has(fsnotify.Remove) || ev.Op.Has(fsnotify.Rename) {
		if !strings.HasSuffix(rel, ".md") {
			// possibly a removed directory: queue every indexed note under it
			// so applyBatch's stat sees them gone and drops them
			relevant := false
			for _, p := range w.ix.pathsUnder(rel + "/") {
				pending[p] = struct{}{}
				relevant = true
			}
			return relevant
		}
	}

	if !strings.HasSuffix(rel, ".md") {
		return false
	}
	pending[rel] = struct{}{}
	return true
}
