package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type searchResponse struct {
	Results []struct {
		Path    string  `json:"path"`
		Title   string  `json:"title"`
		Tier    string  `json:"tier"`
		Score   float64 `json:"score"`
		Matches []struct {
			Line    int    `json:"line"`
			Snippet string `json:"snippet"`
		} `json:"matches"`
	} `json:"results"`
}

func doSearch(t *testing.T, query string) (int, string, searchResponse) {
	t.Helper()
	rec := get(t, "/v1/search?"+query)
	var resp searchResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, rec.Body.String(), resp
}

func TestSearchBody(t *testing.T) {
	code, raw, resp := doSearch(t, "q="+url.QueryEscape("does not change"))
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	// Non-negotiable #3: snippets, never bodies.
	if strings.Contains(raw, `"body"`) {
		t.Fatal("/search response contains a body field")
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != "engineering/basim/soul.md" {
		t.Fatalf("results = %+v", resp.Results)
	}
	r := resp.Results[0]
	if r.Title != "soul" || r.Score <= 0 || r.Score > 1 {
		t.Fatalf("hit = %+v", r)
	}
	if len(r.Matches) == 0 || r.Matches[0].Line != 9 || !strings.Contains(r.Matches[0].Snippet, "does not change") {
		t.Fatalf("matches = %+v", r.Matches)
	}
}

// TestSearchResultsCarryTier is spec/tiers.md's explicit requirement:
// tier appears in every /search response entry.
func TestSearchResultsCarryTier(t *testing.T) {
	_, _, resp := doSearch(t, "q="+url.QueryEscape("does not change"))
	if len(resp.Results) == 0 {
		t.Fatal("expected at least one result to check")
	}
	for _, r := range resp.Results {
		if r.Tier == "" {
			t.Fatalf("result %s has no tier", r.Path)
		}
	}
}

func TestSearchTierFilter(t *testing.T) {
	code, _, resp := doSearch(t, "q="+url.QueryEscape("does not change")+"&tier=asil")
	if code != http.StatusOK || len(resp.Results) != 0 {
		t.Fatalf("tier=asil should exclude every fixture note: code=%d results=%+v", code, resp.Results)
	}

	code, _, _ = doSearch(t, "q=x&tier=not-a-real-tier")
	if code != http.StatusBadRequest {
		t.Fatalf("unknown tier: status = %d, want 400", code)
	}
}

func TestSearchDiacriticFolding(t *testing.T) {
	code, raw, resp := doSearch(t, "q=sirr")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != "philosophy/barzakh.md" {
		t.Fatalf("`sirr` should hit barzakh.md: %+v", resp.Results)
	}
}

func TestSearchTitleAndFilters(t *testing.T) {
	// in=title
	code, _, resp := doSearch(t, "q=soul&in=title")
	if code != http.StatusOK || len(resp.Results) != 1 || resp.Results[0].Path != "engineering/basim/soul.md" {
		t.Fatalf("title search: code=%d results=%+v", code, resp.Results)
	}
	// soul.md carries no tier field, so it's dakhil by default — a title
	// match scores 1.0 before the tier weight (0.4 for dakhil) is applied.
	if resp.Results[0].Score != 0.4 {
		t.Fatalf("title match score = %v, want 0.4", resp.Results[0].Score)
	}

	// folder filter excludes the hit
	code, _, resp = doSearch(t, "q=sirr&folder=engineering/")
	if code != http.StatusOK || len(resp.Results) != 0 {
		t.Fatalf("folder filter leaked: %+v", resp.Results)
	}

	// tag filter keeps it
	code, _, resp = doSearch(t, "q=sirr&tag=philosophy")
	if code != http.StatusOK || len(resp.Results) != 1 {
		t.Fatalf("tag filter dropped the hit: %+v", resp.Results)
	}
}

func TestSearchValidation(t *testing.T) {
	if code, _, _ := doSearch(t, ""); code != http.StatusBadRequest {
		t.Fatalf("missing q: status = %d", code)
	}
	if code, _, _ := doSearch(t, "q=x&in=nope"); code != http.StatusBadRequest {
		t.Fatalf("bad in: status = %d", code)
	}
	if code, _, _ := doSearch(t, "q=x&limit=0"); code != http.StatusBadRequest {
		t.Fatalf("bad limit: status = %d", code)
	}
}
