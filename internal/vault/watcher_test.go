package vault

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testDebounce = 50 * time.Millisecond

func startWatcher(t *testing.T, ix *Index) {
	t.Helper()
	w, err := NewWatcher(ix, testDebounce)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestWatcherEditWithoutRestart(t *testing.T) {
	root := t.TempDir()
	writeVaultFile(t, root, "a.md", "# a\n\nfirst version\n")

	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	startWatcher(t, ix)

	// create
	writeVaultFile(t, root, "sub/new.md", "# new\n\nlinks to [[a]]\n")
	eventually(t, "new note indexed", func() bool {
		_, ok := ix.Get("sub/new.md")
		return ok
	})
	eventually(t, "backlink from new note", func() bool {
		return len(ix.Backlinks("a.md")) == 1
	})

	// modify
	writeVaultFile(t, root, "a.md", "# a\n\n## Added Section\n\nsecond version\n")
	eventually(t, "modified note reparsed", func() bool {
		n, ok := ix.Get("a.md")
		return ok && len(n.Headings) == 2
	})

	// delete
	if err := os.Remove(filepath.Join(root, "sub", "new.md")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted note dropped and edges cleaned", func() bool {
		_, ok := ix.Get("sub/new.md")
		return !ok && len(ix.Backlinks("a.md")) == 0
	})

	// hidden files never enter the index
	writeVaultFile(t, root, ".secret/x.md", "# x\n")
	writeVaultFile(t, root, "visible.md", "# visible\n")
	eventually(t, "visible note indexed", func() bool {
		_, ok := ix.Get("visible.md")
		return ok
	})
	if _, ok := ix.Get(".secret/x.md"); ok {
		t.Fatal("hidden note was indexed")
	}
}

func TestWatcherGitPullBurst(t *testing.T) {
	root := t.TempDir()
	writeVaultFile(t, root, "seed.md", "# seed\n")

	ix := NewIndex(root, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	startWatcher(t, ix)

	const n = 50
	for i := range n {
		writeVaultFile(t, root, fmt.Sprintf("pull/note%02d.md", i),
			fmt.Sprintf("# note%02d\n\nlinks to [[seed]]\n", i))
	}

	eventually(t, "all 50 notes indexed", func() bool {
		return ix.Len() == n+1
	})
	eventually(t, "all 50 backlinks present", func() bool {
		return len(ix.Backlinks("seed.md")) == n
	})

	// let any trailing debounce windows flush before counting
	time.Sleep(4 * testDebounce)

	// Bounded reparses: each file may be parsed a couple of times if the
	// burst straddles debounce windows, but nothing like once per event.
	if got := ix.Reparses(); got > 2*n {
		t.Fatalf("burst caused %d reparses for %d files — debounce is thrashing", got, n)
	}
}
