package search

import (
	"context"
	"testing"
)

const fixtureVault = "../../testdata/vault"

func newSearcher(t *testing.T) *Searcher {
	t.Helper()
	s, err := New(fixtureVault)
	if err != nil {
		t.Fatalf("New: %v (is ripgrep installed?)", err)
	}
	return s
}

func TestSearch(t *testing.T) {
	s := newSearcher(t)
	tests := []struct {
		name     string
		q        string
		wantPath string // must appear in results
		wantLine int
	}{
		{"plain ascii", "frontmatter", "plain.md", 3},
		{"case-insensitive", "FRONTMATTER", "plain.md", 3},
		{"diacritic folded ascii query", "sirr", "philosophy/barzakh.md", 9},
		{"diacritic query", "Şirr", "philosophy/barzakh.md", 9},
		{"folded both ways", "evvel-axir", "philosophy/barzakh.md", 9},
		{"multi word", "does not change", "engineering/basim/soul.md", 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results, err := s.Search(context.Background(), tt.q)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range results {
				if r.Path != tt.wantPath {
					continue
				}
				for _, m := range r.Matches {
					if m.Line == tt.wantLine {
						if m.Snippet == "" {
							t.Fatal("empty snippet")
						}
						return
					}
				}
				t.Fatalf("path hit but wrong lines: %+v", r.Matches)
			}
			t.Fatalf("no hit for %q in %s; results: %+v", tt.q, tt.wantPath, results)
		})
	}
}

func TestSearchNoMatchesIsEmptyNotError(t *testing.T) {
	results, err := newSearcher(t).Search(context.Background(), "zzz-no-such-string-zzz")
	if err != nil {
		t.Fatalf("no matches should not error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty results, got %+v", results)
	}
}

func TestSearchSkipsHiddenFiles(t *testing.T) {
	// .hidden/secret.md contains "never be indexed"
	results, err := newSearcher(t).Search(context.Background(), "never be indexed")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Path == ".hidden/secret.md" {
			t.Fatal("search surfaced a hidden file")
		}
	}
}

func TestQueryIsNeverRegex(t *testing.T) {
	// a regex metacharacter query must not blow up or match everything
	results, err := newSearcher(t).Search(context.Background(), ".*")
	if err != nil {
		t.Fatalf("metacharacter query errored: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("`.*` should match nothing as a literal, got %d results", len(results))
	}
}
