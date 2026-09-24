package render

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"null-service/internal/search"
	"null-service/internal/vault"
)

// writableFixture copies the fixture vault into a fresh git repository
// and returns a router over it with writes enabled, plus its root and
// index — the renderer's write paths are never exercised against the
// committed fixture.
func writableFixture(t *testing.T) (http.Handler, string, *vault.Index) {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("cp", "-r", fixtureVault+"/.", root).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "seed"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	log := slog.New(slog.DiscardHandler)
	ix := vault.NewIndex(root, log)
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	s, err := search.New(root)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := New(ix, s, root, "secret", log)
	if err != nil {
		t.Fatal(err)
	}
	rd.Writable = true
	r := chi.NewRouter()
	rd.Mount(r)
	return r, root, ix
}

func do(h http.Handler, method, target string, form url.Values, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret"})
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func userNavigation(r *http.Request) {
	r.Header.Set("Sec-Fetch-User", "?1")
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
}

func noteTier(t *testing.T, ix *vault.Index, p string) vault.Tier {
	t.Helper()
	n, ok := ix.Get(p)
	if !ok {
		t.Fatalf("no note %s", p)
	}
	return n.Tier
}

func TestHeaderShowsTierCountsAndPort(t *testing.T) {
	body := getPage(t, testRouter(t), "/", true).Body.String()
	for _, want := range []string{`class="tiers"`, `t-dakhil`, "<b>6</b>", `href="/graph"`, `href="/al-mina"`} {
		if !strings.Contains(body, want) {
			t.Errorf("header missing %q", want)
		}
	}
}

func TestListTierFilter(t *testing.T) {
	h := testRouter(t)
	if body := getPage(t, h, "/?tier=dakhil", true).Body.String(); !strings.Contains(body, "6 notes") {
		t.Error("tier=dakhil should list the whole (all-dakhil) fixture")
	}
	if body := getPage(t, h, "/?tier=asil", true).Body.String(); !strings.Contains(body, "0 notes") {
		t.Error("tier=asil should list nothing")
	}
}

func TestEveryPageCarriesCSP(t *testing.T) {
	h := testRouter(t)
	for _, target := range []string{"/", "/n/plain.md", "/graph", "/al-mina", "/s?q=x"} {
		csp := getPage(t, h, target, true).Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "frame-src 'none'") {
			t.Errorf("%s: weak or missing CSP %q", target, csp)
		}
	}
}

func TestGraphPageAndCompactData(t *testing.T) {
	h := testRouter(t)
	if body := getPage(t, h, "/graph", true).Body.String(); !strings.Contains(body, `src="/static/graph.js"`) {
		t.Error("graph page must load the graph script")
	}
	rec := getPage(t, h, "/graph/data", true)
	raw := rec.Body.String()
	if strings.Contains(raw, "\n") || strings.Contains(raw, `":  `) {
		t.Fatalf("graph data is not compact JSON: %q", raw[:min(80, len(raw))])
	}
	var g struct {
		Nodes []struct{ Path, Title, Tier string } `json:"nodes"`
		Edges []struct{ From, To, Tier string }    `json:"edges"`
	}
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 6 || len(g.Edges) == 0 {
		t.Fatalf("nodes=%d edges=%d", len(g.Nodes), len(g.Edges))
	}
	for _, n := range g.Nodes {
		if n.Tier == "" {
			t.Errorf("node %s has no tier", n.Path)
		}
	}
	for _, e := range g.Edges {
		if e.Tier == "" || strings.HasPrefix(e.From, ".hidden") {
			t.Errorf("bad edge %+v", e)
		}
	}
}

func TestStaticScriptsServed(t *testing.T) {
	h := testRouter(t)
	for _, f := range []string{"style.css", "graph.js", "mina.js"} {
		if rec := getPage(t, h, "/static/"+f, true); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("/static/%s: %d", f, rec.Code)
		}
	}
	if rec := getPage(t, h, "/static/graph.js", false); rec.Code != http.StatusUnauthorized {
		t.Error("static files must sit behind auth too")
	}
}

func TestFirstOpenPromotesOnlyGenuineUserNavigation(t *testing.T) {
	h, _, ix := writableFixture(t)
	const p = "plain.md"

	// none of these can promote: a model-planted <img>, an API client with
	// the bearer header, a prefetch, a client sending no Sec-Fetch headers
	do(h, "GET", "/n/"+p, nil, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Dest", "image")
		r.Header.Set("Sec-Fetch-Mode", "no-cors")
	})
	do(h, "GET", "/n/"+p, nil, func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", "iframe") })
	do(h, "GET", "/n/"+p, nil, func(r *http.Request) { userNavigation(r); r.Header.Del("Sec-Fetch-User") }) // meta-refresh / script nav
	do(h, "GET", "/n/"+p, nil)                                                                              // curl-like, cookie but no fetch metadata
	do(h, "GET", "/n/"+p, nil, func(r *http.Request) {                                                      // header auth, no cookie
		r.Header.Del("Cookie")
		r.Header.Set("Authorization", "Bearer secret")
		userNavigation(r)
	})
	if got := noteTier(t, ix, p); got != vault.TierDakhil {
		t.Fatalf("tier = %s after non-human reads, want dakhil", got)
	}

	rec := do(h, "GET", "/n/"+p, nil, userNavigation)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Marked <b>amil</b>") {
		t.Fatalf("user navigation should promote and say so: %d", rec.Code)
	}
	if got := noteTier(t, ix, p); got != vault.TierAmil {
		t.Fatalf("tier = %s, want amil after a real open", got)
	}
}

func TestReadOnlyRendererNeverWrites(t *testing.T) {
	h := testRouter(t) // Writable is false, and the fixture vault is not a git repo
	rec := getPage(t, h, "/n/plain.md", true)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	// a POST is refused outright rather than half-applied
	rec = do(h, "POST", "/al-mina/act", url.Values{"csrf": {"x"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST on a read-only renderer = %d, want 503", rec.Code)
	}
}

// proposal puts an outstanding tier proposal on rel by way of the same
// write path the model uses.
func proposal(t *testing.T, root, rel string, tier vault.Tier) {
	t.Helper()
	if err := vault.ProposeTier(root, rel, tier, "looks settled"); err != nil {
		t.Fatal(err)
	}
}

func TestMinaPageListsProposalsWithReasons(t *testing.T) {
	h, root, ix := writableFixture(t)
	proposal(t, root, "index.md", vault.TierAmil)
	ix.Refresh("index.md")
	body := do(h, "GET", "/al-mina", nil).Body.String()
	for _, want := range []string{"looks settled", `name="act:index.md"`, `value="approve"`, `name="csrf"`, "/static/mina.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("Al-Mina page missing %q", want)
		}
	}
}

func TestMinaBatchAppliesEachDecisionAsItsOwnCommit(t *testing.T) {
	h, root, ix := writableFixture(t)
	for _, p := range []string{"index.md", "plain.md", "malformed.md"} {
		proposal(t, root, p, vault.TierAmil)
		ix.Refresh(p)
	}
	page := do(h, "GET", "/al-mina", nil).Body.String()
	i := strings.Index(page, `name="csrf" value="`)
	csrf := page[i+len(`name="csrf" value="`):]
	csrf = csrf[:strings.Index(csrf, `"`)]

	before := gitCount(t, root)
	rec := do(h, "POST", "/al-mina/act", url.Values{
		"csrf":                {csrf},
		"act:index.md":        {"approve"},
		"act:plain.md":        {"deny"},
		"reason:plain.md":     {"too thin"},
		"act:malformed.md":    {"defer"},
		"act:soul-not-listed": {"skip"},
	})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "done=3") {
		t.Fatalf("batch = %d -> %s", rec.Code, rec.Header().Get("Location"))
	}
	if got := gitCount(t, root); got != before+3 {
		t.Fatalf("commits = %d, want %d: one per note", got, before+3)
	}
	if got := noteTier(t, ix, "index.md"); got != vault.TierAmil {
		t.Errorf("approved note tier = %s", got)
	}
	n, _ := ix.Get("plain.md")
	if d := vault.DenialFrom(n.Frontmatter); d == nil || d.Reason != "too thin" || vault.ProposalFrom(n.Frontmatter) != nil {
		t.Errorf("denied note: %+v", n.Frontmatter)
	}
	m, _ := ix.Get("malformed.md")
	if vault.ProposalFrom(m.Frontmatter) != nil || vault.DenialFrom(m.Frontmatter) != nil {
		t.Errorf("deferred note should have no proposal and no denial")
	}
}

func gitCount(t *testing.T, root string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range strings.TrimSpace(string(out)) {
		n = n*10 + int(c-'0')
	}
	return n
}

// The threat this guards: a model writes a note containing an HTML form
// that posts an approval. Same-origin, cookie attached — only the CSRF
// token (which a note cannot learn) and Sec-Fetch-Site stand in the way.
func TestMinaRejectsForgedPosts(t *testing.T) {
	h, root, ix := writableFixture(t)
	proposal(t, root, "index.md", vault.TierAsil)
	ix.Refresh("index.md")

	forge := func(csrf string, mods ...func(*http.Request)) int {
		return do(h, "POST", "/al-mina/act", url.Values{"csrf": {csrf}, "act:index.md": {"approve"}}, mods...).Code
	}
	if c := forge(""); c != http.StatusForbidden {
		t.Errorf("missing token: %d", c)
	}
	if c := forge("guess"); c != http.StatusForbidden {
		t.Errorf("wrong token: %d", c)
	}
	page := do(h, "GET", "/al-mina", nil).Body.String()
	i := strings.Index(page, `name="csrf" value="`) + len(`name="csrf" value="`)
	good := page[i : i+strings.Index(page[i:], `"`)]
	if c := forge(good, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); c != http.StatusForbidden {
		t.Errorf("cross-site with a valid token: %d", c)
	}
	if c := forge(good, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); c != http.StatusForbidden {
		t.Errorf("foreign Origin: %d", c)
	}
	if got := noteTier(t, ix, "index.md"); got == vault.TierAsil {
		t.Fatal("a forged POST raised a tier")
	}
}
