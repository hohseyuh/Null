package vault

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fixtureVault = "../../testdata/vault"

// fixturePaths is the complete set of indexable notes in the fixture vault.
// .hidden/secret.md is deliberately absent.
var fixturePaths = []string{
	"index.md",
	"plain.md",
	"malformed.md",
	"engineering/basim/soul.md",
	"engineering/basim/character.md",
	"philosophy/barzakh.md",
}

func parseFixture(t *testing.T, relPath string) *Note {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureVault, filepath.FromSlash(relPath)))
	if err != nil {
		t.Fatalf("read fixture %s: %v", relPath, err)
	}
	n := Parse(relPath, raw, time.Unix(1_755_600_000, 0), slog.New(slog.DiscardHandler))
	n.ResolveLinks(NewResolver(fixturePaths))
	return n
}

func TestParse(t *testing.T) {
	tests := []struct {
		path         string
		title        string
		frontmatter  map[string]any
		tags         []string
		bodyLine     int
		headings     []Heading
		outlinks     []Link
		bodyContains string
	}{
		{
			path:  "engineering/basim/soul.md",
			title: "soul",
			frontmatter: map[string]any{
				"status": "active", "type": "spec", "tags": []any{"basim", "spec"},
			},
			tags:     []string{"basim", "spec"},
			bodyLine: 6,
			headings: []Heading{
				{Text: "soul", Level: 1, Line: 7},
				{Text: "Who I am", Level: 2, Line: 11},
				{Text: "Failure modes", Level: 2, Line: 15},
			},
			outlinks: []Link{
				{
					Target: "character", Alias: "the character file",
					Path: "engineering/basim/character.md", Resolved: true,
					Line: 13, Context: "The constant beneath [[character|the character file]].",
				},
				{
					Target: "engineering/basim/character.md",
					Path:   "engineering/basim/character.md", Resolved: true,
					Line: 17, Context: "If [[engineering/basim/character.md]] and this file ever disagree, this file wins.",
				},
				{
					Target: "index", Section: "Map",
					Path: "index.md", Resolved: true,
					Line: 18, Context: "See also [[index#Map]].",
				},
			},
			bodyContains: "# soul",
		},
		{
			path:        "plain.md",
			title:       "plain",
			frontmatter: map[string]any{},
			bodyLine:    1,
			headings:    []Heading{{Text: "plain", Level: 1, Line: 1}},
			outlinks: []Link{
				{
					Target: "does-not-exist", Resolved: false,
					Line: 5, Context: "This links to [[does-not-exist]] which is broken.",
				},
			},
			bodyContains: "No frontmatter here.",
		},
		{
			path:        "philosophy/barzakh.md",
			title:       "barzakh",
			frontmatter: map[string]any{"tags": []any{"philosophy"}},
			tags:        []string{"philosophy"},
			bodyLine:    4,
			headings: []Heading{
				{Text: "barzakh", Level: 1, Line: 5},
				{Text: "Aralıq vəziyyət", Level: 2, Line: 7},
			},
			outlinks: []Link{
				{
					Target: "soul", Path: "engineering/basim/soul.md", Resolved: true,
					Line: 11, Context: "Bax: [[soul]] və [[plain]].",
				},
				{
					Target: "plain", Path: "plain.md", Resolved: true,
					Line: 11, Context: "Bax: [[soul]] və [[plain]].",
				},
			},
			bodyContains: "əvvəl-axır",
		},
		{
			path:         "malformed.md",
			title:        "malformed",
			frontmatter:  map[string]any{}, // malformed YAML degrades, never fails
			bodyLine:     5,
			headings:     []Heading{{Text: "malformed", Level: 1, Line: 6}},
			bodyContains: "Body survives",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			n := parseFixture(t, tt.path)
			if n.Title != tt.title {
				t.Errorf("Title = %q, want %q", n.Title, tt.title)
			}
			if !reflect.DeepEqual(n.Frontmatter, tt.frontmatter) {
				t.Errorf("Frontmatter = %#v, want %#v", n.Frontmatter, tt.frontmatter)
			}
			if !reflect.DeepEqual(n.Tags, tt.tags) {
				t.Errorf("Tags = %#v, want %#v", n.Tags, tt.tags)
			}
			if n.BodyLine != tt.bodyLine {
				t.Errorf("BodyLine = %d, want %d", n.BodyLine, tt.bodyLine)
			}
			if !reflect.DeepEqual(n.Headings, tt.headings) {
				t.Errorf("Headings = %#v, want %#v", n.Headings, tt.headings)
			}
			if !reflect.DeepEqual(n.Outlinks, tt.outlinks) {
				t.Errorf("Outlinks = %#v, want %#v", n.Outlinks, tt.outlinks)
			}
			if !strings.Contains(n.Body, tt.bodyContains) {
				t.Errorf("Body does not contain %q", tt.bodyContains)
			}
			if strings.Contains(n.Body, "\n---\n") && tt.path != "plain.md" {
				// frontmatter fences must not leak into the body
				t.Errorf("Body appears to contain a frontmatter fence")
			}
		})
	}
}

func TestResolver(t *testing.T) {
	r := NewResolver(fixturePaths)
	tests := []struct {
		target string
		want   string
		ok     bool
	}{
		{"engineering/basim/soul.md", "engineering/basim/soul.md", true},
		{"engineering/basim/soul", "engineering/basim/soul.md", true},
		{"soul", "engineering/basim/soul.md", true},
		{"soul.md", "engineering/basim/soul.md", true},
		{"philosophy/barzakh", "philosophy/barzakh.md", true},
		{"index", "index.md", true},
		{"does-not-exist", "", false},
	}
	for _, tt := range tests {
		got, ok := r.Resolve(tt.target)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Resolve(%q) = %q,%v want %q,%v", tt.target, got, ok, tt.want, tt.ok)
		}
	}
}

func TestSplitFrontmatterNoClosingFence(t *testing.T) {
	raw := []byte("---\ntitle: open\nnothing closes this\n")
	fm, body, bodyLine := splitFrontmatter(raw)
	if fm != nil || bodyLine != 1 || string(body) != string(raw) {
		t.Fatalf("unclosed fence should yield all-body, got fm=%q bodyLine=%d", fm, bodyLine)
	}
}
