package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer secret")
	testServer().Router().ServeHTTP(rec, req)
	return rec
}

func TestListNotesMetadataOnly(t *testing.T) {
	rec := get(t, "/v1/notes")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// Non-negotiable #3: no body field, ever, anywhere in a list response.
	if strings.Contains(rec.Body.String(), `"body"`) {
		t.Fatal("/notes response contains a body field")
	}
	var resp struct {
		Notes []struct {
			Path          string `json:"path"`
			Title         string `json:"title"`
			OutlinkCount  int    `json:"outlink_count"`
			BacklinkCount int    `json:"backlink_count"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Notes) != 6 {
		t.Fatalf("got %d notes, want 6", len(resp.Notes))
	}
	for _, n := range resp.Notes {
		if n.Path == "engineering/basim/soul.md" {
			if n.Title != "soul" || n.OutlinkCount != 3 || n.BacklinkCount != 3 {
				t.Fatalf("soul summary wrong: %+v", n)
			}
			return
		}
	}
	t.Fatal("soul.md not listed")
}

// TestNotesCarryTier is spec/tiers.md's explicit requirement: tier
// appears in every /notes response entry.
func TestNotesCarryTier(t *testing.T) {
	rec := get(t, "/v1/notes")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Notes []struct {
			Path string `json:"path"`
			Tier string `json:"tier"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, n := range resp.Notes {
		if n.Tier == "" {
			t.Fatalf("note %s has no tier in the response", n.Path)
		}
	}
}

func TestListNotesTierFilter(t *testing.T) {
	// the fixture vault carries no tier frontmatter anywhere, so every
	// note defaults to dakhil
	rec := get(t, "/v1/notes?tier=dakhil")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Notes []struct{ Path string } `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Notes) != 6 {
		t.Fatalf("tier=dakhil returned %d notes, want 6 (the whole fixture vault)", len(resp.Notes))
	}

	rec = get(t, "/v1/notes?tier=asil")
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Notes) != 0 {
		t.Fatalf("tier=asil returned %d notes, want 0", len(resp.Notes))
	}

	rec = get(t, "/v1/notes?tier=not-a-real-tier")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown tier: status = %d, want 400", rec.Code)
	}
}

func TestListNotesFilters(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string // exact set of paths, any order
	}{
		{"folder", "folder=engineering/", []string{
			"engineering/basim/soul.md", "engineering/basim/character.md"}},
		{"folder no slash", "folder=philosophy", []string{"philosophy/barzakh.md"}},
		{"tag", "tag=basim", []string{
			"engineering/basim/soul.md", "engineering/basim/character.md"}},
		{"tags AND", "tag=basim&tag=spec", []string{"engineering/basim/soul.md"}},
		{"tag nobody has", "tag=nope", nil},
		{"folder and tag", "folder=engineering/&tag=spec", []string{"engineering/basim/soul.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, "/v1/notes?"+tt.query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			var resp struct {
				Notes []struct {
					Path string `json:"path"`
				} `json:"notes"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, n := range resp.Notes {
				got = append(got, n.Path)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("paths = %v, want %v", got, tt.want)
			}
			for _, w := range tt.want {
				found := false
				for _, g := range got {
					if g == w {
						found = true
					}
				}
				if !found {
					t.Fatalf("paths = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestListNotesCursorPagination(t *testing.T) {
	var all []string
	cursor := ""
	for range 10 {
		target := "/v1/notes?sort=path&limit=2"
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := get(t, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		var resp struct {
			Notes []struct {
				Path string `json:"path"`
			} `json:"notes"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Notes) > 2 {
			t.Fatalf("page larger than limit: %d", len(resp.Notes))
		}
		for _, n := range resp.Notes {
			all = append(all, n.Path)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	if len(all) != 6 {
		t.Fatalf("paginated walk returned %d notes, want 6: %v", len(all), all)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] >= all[i] {
			t.Fatalf("pages not in path order: %v", all)
		}
	}
}

func TestGetNote(t *testing.T) {
	rec := get(t, "/v1/notes/engineering/basim/soul.md?include=body,outlinks,backlinks")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Path        string         `json:"path"`
		Frontmatter map[string]any `json:"frontmatter"`
		Body        string         `json:"body"`
		Headings    []struct {
			Text  string `json:"text"`
			Level int    `json:"level"`
			Line  int    `json:"line"`
		} `json:"headings"`
		Outlinks  []string `json:"outlinks"`
		Backlinks []string `json:"backlinks"`
		UpdatedAt string   `json:"updated_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Path != "engineering/basim/soul.md" {
		t.Fatalf("path = %q", resp.Path)
	}
	if resp.Frontmatter["status"] != "active" {
		t.Fatalf("frontmatter = %v", resp.Frontmatter)
	}
	if !strings.Contains(resp.Body, "# soul") || strings.Contains(resp.Body, "status: active") {
		t.Fatalf("body wrong (frontmatter leaked or content missing): %q", resp.Body)
	}
	if len(resp.Headings) != 3 || resp.Headings[2].Text != "Failure modes" || resp.Headings[2].Line != 15 {
		t.Fatalf("headings = %+v", resp.Headings)
	}
	if len(resp.Outlinks) != 2 { // character.md deduped, index.md
		t.Fatalf("outlinks = %v", resp.Outlinks)
	}
	if len(resp.Backlinks) != 3 {
		t.Fatalf("backlinks = %v", resp.Backlinks)
	}
	if resp.UpdatedAt == "" {
		t.Fatal("updated_at missing")
	}
}

func TestGetNoteDefaultIncludeIsBodyOnly(t *testing.T) {
	rec := get(t, "/v1/notes/plain.md")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"body"`) {
		t.Fatal("default include should return the body")
	}
	if strings.Contains(body, `"outlinks"`) || strings.Contains(body, `"backlinks"`) {
		t.Fatal("default include should not return links")
	}
}

func TestGetNoteSection(t *testing.T) {
	rec := get(t, "/v1/notes/engineering/basim/soul.md?section="+url.QueryEscape("Failure modes"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Body, "## Failure modes") {
		t.Fatalf("section should start at its heading: %q", resp.Body)
	}
	if strings.Contains(resp.Body, "Who I am") {
		t.Fatalf("section leaked the previous section: %q", resp.Body)
	}

	// unknown heading -> 404
	rec = get(t, "/v1/notes/engineering/basim/soul.md?section=Nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown section: status = %d", rec.Code)
	}
}

func TestGetNoteBodyCap(t *testing.T) {
	s := testServer()
	s.MaxBodyBytes = 10
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/notes/plain.md", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "section") {
		t.Fatalf("413 detail should point at ?section=: %s", rec.Body)
	}
}

func TestGetNotePathSafety(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   int
	}{
		{"traversal", "/v1/notes/../../etc/passwd", http.StatusBadRequest},
		{"deep traversal", "/v1/notes/a/../../../etc/passwd", http.StatusBadRequest},
		{"absolute", "/v1/notes//etc/passwd", http.StatusBadRequest},
		{"encoded traversal", "/v1/notes/%2e%2e%2f%2e%2e%2fetc%2fpasswd", http.StatusBadRequest},
		{"encoded absolute", "/v1/notes/%2fetc%2fpasswd", http.StatusBadRequest},
		{"backslash", "/v1/notes/..%5c..%5cetc%5cpasswd", http.StatusBadRequest},
		{"hidden dir", "/v1/notes/.hidden/secret.md", http.StatusNotFound},
		{"dotgit", "/v1/notes/.git/config", http.StatusNotFound},
		{"missing note", "/v1/notes/nope.md", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, tt.target)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}
