package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

func TestGraphRoute(t *testing.T) {
	rec := get(t, "/v1/graph?path="+url.QueryEscape("engineering/basim/character.md")+"&depth=2&direction=out")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Root  string `json:"root"`
		Nodes []struct {
			Path     string `json:"path"`
			Title    string `json:"title"`
			Distance int    `json:"distance"`
		} `json:"nodes"`
		Edges []struct {
			From    string `json:"from"`
			To      string `json:"to"`
			Context string `json:"context"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Root != "engineering/basim/character.md" {
		t.Fatalf("root = %q", resp.Root)
	}
	if len(resp.Nodes) != 2 || len(resp.Edges) != 4 {
		t.Fatalf("nodes=%d edges=%d, want 2 and 4", len(resp.Nodes), len(resp.Edges))
	}
	for _, e := range resp.Edges {
		if e.Context == "" {
			t.Fatalf("edge %s->%s missing context", e.From, e.To)
		}
	}
}

func TestGraphValidation(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   int
	}{
		{"missing path", "/v1/graph", http.StatusBadRequest},
		{"depth too deep", "/v1/graph?path=plain.md&depth=4", http.StatusBadRequest},
		{"depth zero", "/v1/graph?path=plain.md&depth=0", http.StatusBadRequest},
		{"bad direction", "/v1/graph?path=plain.md&direction=sideways", http.StatusBadRequest},
		{"unknown note", "/v1/graph?path=nope.md", http.StatusNotFound},
		{"hidden note", "/v1/graph?path=.hidden/secret.md", http.StatusNotFound},
		{"traversal", "/v1/graph?path=../etc/passwd", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, tt.target)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}
