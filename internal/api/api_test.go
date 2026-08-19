package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"null-service/internal/vault"
)

const fixtureVault = "../../testdata/vault"

var fixtureIndex = sync.OnceValue(func() *vault.Index {
	ix := vault.NewIndex(fixtureVault, slog.New(slog.DiscardHandler))
	if err := ix.Build(); err != nil {
		panic(err)
	}
	return ix
})

func testServer() *Server {
	return &Server{
		Token:        "secret",
		MaxBodyBytes: 200_000,
		VaultRoot:    fixtureVault,
		Index:        fixtureIndex(),
		Log:          slog.New(slog.DiscardHandler),
	}
}

func TestHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	testServer().Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Status       string `json:"status"`
		NotesIndexed int    `json:"notes_indexed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Status != "ok" || body.NotesIndexed != 6 {
		t.Fatalf("body = %+v, want status=ok notes_indexed=6", body)
	}
}

func TestRequireBearer(t *testing.T) {
	h := testServer().Router()

	tests := []struct {
		name   string
		path   string
		header string
		want   int
	}{
		{"no header", "/v1/notes", "", http.StatusUnauthorized},
		{"wrong scheme", "/v1/notes", "Basic secret", http.StatusUnauthorized},
		{"wrong token", "/v1/notes", "Bearer wrong", http.StatusUnauthorized},
		{"token prefix", "/v1/notes", "Bearer secre", http.StatusUnauthorized},
		{"token with suffix", "/v1/notes", "Bearer secrets", http.StatusUnauthorized},
		{"unknown route unauthenticated", "/anything", "", http.StatusUnauthorized},
		{"unknown route authenticated is 404", "/anything", "Bearer secret", http.StatusNotFound},
		{"health needs no token", "/v1/health", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}
