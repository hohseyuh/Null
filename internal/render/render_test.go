package render

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"null-service/internal/search"
	"null-service/internal/vault"
)

const fixtureVault = "../../testdata/vault"

func testRouter(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	ix := vault.NewIndex(fixtureVault, log)
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	s, err := search.New(fixtureVault)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := New(ix, s, fixtureVault, "secret", log)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	rd.Mount(r)
	return r
}

func getPage(t *testing.T, h http.Handler, target string, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if authed {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret"})
	}
	h.ServeHTTP(rec, req)
	return rec
}

func TestUnauthenticatedGets401(t *testing.T) {
	h := testRouter(t)
	for _, target := range []string{"/", "/n/plain.md", "/s?q=x", "/static/style.css"} {
		if rec := getPage(t, h, target, false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated: status = %d, want 401", target, rec.Code)
		}
	}
}

func TestLoginSetsCookie(t *testing.T) {
	h := testRouter(t)

	rec := getPage(t, h, "/login?token=wrong", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token login: status = %d", rec.Code)
	}

	rec = getPage(t, h, "/login?token=secret", false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: status = %d", rec.Code)
	}
	cookie := rec.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != cookieName || cookie[0].Value != "secret" || !cookie[0].HttpOnly {
		t.Fatalf("cookie = %+v", cookie)
	}
}

func TestListPage(t *testing.T) {
	rec := getPage(t, testRouter(t), "/", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"6 notes",
		`href="/n/engineering/basim/soul.md"`,
		"engineering/basim", // folder group heading
		`class="tag"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list page missing %q", want)
		}
	}
	if strings.Contains(body, ".hidden") {
		t.Error("list page leaked a hidden path")
	}
}

func TestNotePageWikilinksAndBacklinks(t *testing.T) {
	h := testRouter(t)
	rec := getPage(t, h, "/n/engineering/basim/soul.md", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// resolved wikilink -> clickable anchor with the alias as its text
	if !strings.Contains(body, `<a href="/n/engineering/basim/character.md" class="wikilink">the character file</a>`) {
		t.Error("aliased wikilink not rewritten to an anchor")
	}
	// frontmatter as a header block, not raw YAML
	if !strings.Contains(body, "<dt>status</dt><dd>active</dd>") {
		t.Error("frontmatter header block missing")
	}
	if strings.Contains(body, "---") && strings.Contains(body, "status: active") {
		t.Error("raw YAML leaked into the page")
	}
	// backlinks with context lines
	if !strings.Contains(body, "Linked from") ||
		!strings.Contains(body, "Traits change; the [[soul]] does not.") {
		t.Error("backlinks with context missing")
	}

	// unresolved wikilink -> dead-link style, not broken HTML or plain text
	rec = getPage(t, h, "/n/plain.md", true)
	if !strings.Contains(rec.Body.String(), `<span class="dead-link" title="unresolved">does-not-exist</span>`) {
		t.Error("broken wikilink not rendered as dead link")
	}
}

func TestNotePage404(t *testing.T) {
	h := testRouter(t)
	for _, target := range []string{"/n/nope.md", "/n/.hidden/secret.md", "/n/../../etc/passwd"} {
		if rec := getPage(t, h, target, true); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, rec.Code)
		}
	}
}

func TestSearchPageHighlights(t *testing.T) {
	rec := getPage(t, testRouter(t), "/s?q=sirr", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/n/philosophy/barzakh.md"`) {
		t.Error("search hit missing")
	}
	if !strings.Contains(body, "<mark>Şirr</mark>") {
		t.Errorf("match not highlighted: %s", body)
	}
}
