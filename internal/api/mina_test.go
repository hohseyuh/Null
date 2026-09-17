package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMinaHasNoProposalsInTheFixtureVault proves /mina never surfaces a
// proposal entry when no note carries proposed_tier — the fixture vault
// has none. It may still surface stale_dakhil entries, since the
// fixture's on-disk mtimes reflect whenever this checkout happened, not
// "now"; that part isn't asserted here.
func TestMinaHasNoProposalsInTheFixtureVault(t *testing.T) {
	rec := get(t, "/mina")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Entries []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Tier string `json:"tier"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.Entries {
		if e.Type != "stale_dakhil" {
			t.Fatalf("entry %+v: want type stale_dakhil (no note in the fixture vault has a proposal)", e)
		}
		if e.Tier != "dakhil" {
			t.Fatalf("entry %+v: a stale_dakhil entry must itself be dakhil", e)
		}
	}
}

func TestMinaRequiresAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mina", nil)
	testServer().Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
