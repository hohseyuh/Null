package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"null-service/internal/config"
)

const fixtureVault = "../../testdata/vault"

// env builds a browse root containing a copy of the fixture vault at
// <root>/notes, a manager over it, and returns them.
func env(t *testing.T, locked bool) (m *Manager, root, cfgPath string) {
	t.Helper()
	root = t.TempDir()
	if out, err := exec.Command("cp", "-r", fixtureVault, filepath.Join(root, "notes")).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m, err := New(ctx, Options{
		Token: "api-token", UIToken: "ui-token", MaxBodyBytes: 200_000,
		ConfigPath: cfgPath, BrowseRoot: root, EnvLocked: locked,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, root, cfgPath
}

func req(m http.Handler, method, target string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, r)
	return rec
}

var ui = map[string]string{"Authorization": "Bearer ui-token"}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func csrfFrom(t *testing.T, m http.Handler, dir string) string {
	t.Helper()
	body := req(m, "GET", "/setup?dir="+url.QueryEscape(dir), nil, ui).Body.String()
	g := csrfRe.FindStringSubmatch(body)
	if g == nil {
		t.Fatalf("no csrf token on the setup page:\n%s", body)
	}
	return g[1]
}

func TestSetupModeBeforeAVaultIsChosen(t *testing.T) {
	m, _, _ := env(t, false)

	if rec := req(m, "GET", "/", nil, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Errorf("/ = %d -> %q, want a redirect to /setup", rec.Code, rec.Header().Get("Location"))
	}
	rec := req(m, "GET", "/v1/notes", nil, map[string]string{"Authorization": "Bearer api-token"})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not_configured") {
		t.Errorf("/v1/notes = %d %s, want 503 not_configured", rec.Code, rec.Body)
	}
	if rec := req(m, "GET", "/setup", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("/setup unauthenticated = %d, want 401", rec.Code)
	}
	// the API token must not open the admin page when a UI token is set
	if rec := req(m, "GET", "/setup", nil, map[string]string{"Authorization": "Bearer api-token"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("/setup with the API token = %d, want 401", rec.Code)
	}
	if rec := req(m, "GET", "/login?token=ui-token", nil, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("/login in setup mode = %d", rec.Code)
	}
}

func TestChoosingAVaultActivatesAndPersists(t *testing.T) {
	m, root, cfgPath := env(t, false)
	notes := filepath.Join(root, "notes")

	page := req(m, "GET", "/setup", nil, ui)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "notes/") {
		t.Fatalf("setup page = %d: subdir listing missing", page.Code)
	}
	csrf := csrfFrom(t, m, notes)

	if rec := req(m, "POST", "/setup", url.Values{"dir": {notes}}, ui); rec.Code != http.StatusForbidden {
		t.Errorf("POST without csrf = %d, want 403", rec.Code)
	}
	if rec := req(m, "POST", "/setup", url.Values{"dir": {"/etc"}, "csrf": {csrf}}, ui); rec.Code != http.StatusBadRequest {
		t.Errorf("POST outside the browse root = %d, want 400", rec.Code)
	}
	if rec := req(m, "POST", "/setup", url.Values{"dir": {filepath.Join(root, "..", "..")}, "csrf": {csrf}}, ui); rec.Code != http.StatusBadRequest {
		t.Errorf("POST with a .. escape = %d, want 400", rec.Code)
	}
	if m.VaultPath() != "" {
		t.Fatal("a refused POST must not activate anything")
	}

	rec := req(m, "POST", "/setup", url.Values{"dir": {notes}, "csrf": {csrf}}, ui)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid POST = %d: %s", rec.Code, rec.Body)
	}
	if got, _ := filepath.EvalSymlinks(notes); m.VaultPath() != got {
		t.Fatalf("active vault = %q, want %q", m.VaultPath(), got)
	}

	// live without a restart
	rec = req(m, "GET", "/v1/health", nil, nil)
	if !strings.Contains(rec.Body.String(), `"notes_indexed":6`) {
		t.Errorf("health after activation: %s", rec.Body)
	}
	if rec := req(m, "GET", "/", nil, ui); rec.Code != 200 || !strings.Contains(rec.Body.String(), "6 notes") {
		t.Errorf("/ after activation = %d", rec.Code)
	}
	// and remembered
	f, err := config.Load(cfgPath)
	if err != nil || f.VaultPath == "" {
		t.Fatalf("config not saved: %+v %v", f, err)
	}
	// the setup page still works afterwards (changing vaults later)
	if rec := req(m, "GET", "/setup", nil, ui); rec.Code != 200 || !strings.Contains(rec.Body.String(), f.VaultPath) {
		t.Errorf("setup after activation = %d", rec.Code)
	}
}

func TestEnvLockedVaultCannotBeChangedFromTheBrowser(t *testing.T) {
	m, root, cfgPath := env(t, true)
	notes := filepath.Join(root, "notes")
	if err := m.Start(notes); err != nil {
		t.Fatal(err)
	}
	body := req(m, "GET", "/setup", nil, ui).Body.String()
	if !strings.Contains(body, "NULL_VAULT_PATH") || strings.Contains(body, `action="/setup"`) {
		t.Error("locked setup page should explain and offer no form")
	}
	if rec := req(m, "POST", "/setup", url.Values{"dir": {notes}, "csrf": {"x"}}, ui); rec.Code != http.StatusForbidden {
		t.Errorf("POST when locked = %d, want 403", rec.Code)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Error("a locked server must never write the config file")
	}
}

func TestSetupRejectsForeignOrigin(t *testing.T) {
	m, root, _ := env(t, false)
	notes := filepath.Join(root, "notes")
	csrf := csrfFrom(t, m, notes)
	hdr := map[string]string{"Authorization": "Bearer ui-token", "Sec-Fetch-Site": "cross-site"}
	if rec := req(m, "POST", "/setup", url.Values{"dir": {notes}, "csrf": {csrf}}, hdr); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", rec.Code)
	}
}

func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "c.json")

	t.Setenv("NULL_VAULT_PATH", "")
	if p, locked, note := Resolve(cfg); p != "" || locked || note != nil {
		t.Errorf("fresh install: %q %v %v", p, locked, note)
	}
	config.Save(cfg, config.File{VaultPath: dir})
	if p, locked, _ := Resolve(cfg); p != dir || locked {
		t.Errorf("saved: %q %v", p, locked)
	}
	config.Save(cfg, config.File{VaultPath: filepath.Join(dir, "gone")})
	if p, _, note := Resolve(cfg); p != "" || note == nil {
		t.Errorf("vanished saved vault should fall back to setup with a note: %q %v", p, note)
	}
	t.Setenv("NULL_VAULT_PATH", dir)
	if p, locked, _ := Resolve(cfg); p != dir || !locked {
		t.Errorf("env: %q %v", p, locked)
	}
}
