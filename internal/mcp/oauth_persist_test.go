package mcp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oauthUnderTest starts a server over an httptest listener, the way
// TestOAuthFullFlow does (baseURL is only known once the listener is up),
// then optionally turns persistence on.
func oauthUnderTest(t *testing.T, statePath string, log *slog.Logger) (*OAuthServer, *httptest.Server) {
	t.Helper()
	oauth, err := NewOAuthServer("http://placeholder", "secret")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewHTTPHandler(testServer(t), oauth, "secret", log))
	t.Cleanup(ts.Close)
	oauth.baseURL, oauth.resource = ts.URL, ts.URL+HTTPPath
	if statePath != "" {
		oauth.EnablePersistence(statePath, log)
	}
	return oauth, ts
}

// fullFlow registers a client, authorizes with the real secret, and
// exchanges the code, returning the client id and both tokens.
func fullFlow(t *testing.T, oauth *OAuthServer, ts *httptest.Server) (clientID, access, refresh string) {
	t.Helper()
	const redirect = "https://client.example/callback"
	clientID = register(t, ts.URL, redirect)
	verifier, challenge := pkcePair()
	code, _ := authorizeAndGetCode(t, noRedirectClient(t), ts.URL, clientID, redirect, challenge, "s", oauth.resource, "secret")
	if code == "" {
		t.Fatal("no code")
	}
	resp, err := http.PostForm(ts.URL+"/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {redirect}, "resource": {oauth.resource}, "code_verifier": {verifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("token exchange failed: %v %+v", err, tok)
	}
	return clientID, tok.AccessToken, tok.RefreshToken
}

func TestOAuthStateSurvivesARestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sub", "oauth.json")

	first, ts1 := oauthUnderTest(t, state, nil)
	clientID, access, refresh := fullFlow(t, first, ts1)
	origin := ts1.URL

	// "restart": a brand-new server object over the same state file and
	// the same origin. (httptest picks a new port, so pin the origin the
	// way a real restart keeps NULL_MCP_PUBLIC_URL.)
	second, err := NewOAuthServer(origin, "secret")
	if err != nil {
		t.Fatal(err)
	}
	second.EnablePersistence(state, nil)

	if !second.ValidateAccessToken(access) {
		t.Error("an access token issued before the restart should still validate")
	}
	if second.ValidateAccessToken("not-a-token") {
		t.Error("garbage validated")
	}
	if _, known := second.clients[clientID]; !known {
		t.Error("a registered client should survive the restart — otherwise its next /authorize is a dead-end 400")
	}
	if _, ok := second.refreshTokens[tokenKey(refresh)]; !ok {
		t.Error("the refresh token should survive the restart")
	}
}

func TestOAuthStateFileHoldsNoUsableSecrets(t *testing.T) {
	state := filepath.Join(t.TempDir(), "oauth.json")
	oauth, ts := oauthUnderTest(t, state, nil)
	_, access, refresh := fullFlow(t, oauth, ts)

	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string]string{"access token": access, "refresh token": refresh, "the shared secret": "secret"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("state file contains the raw %s", name)
		}
	}
	if st, _ := os.Stat(state); st.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestOAuthRotatedRefreshTokenStaysSpentAcrossARestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "oauth.json")
	oauth, ts := oauthUnderTest(t, state, nil)
	clientID, _, refresh := fullFlow(t, oauth, ts)

	resp, err := http.PostForm(ts.URL+"/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}, "resource": {oauth.resource},
	})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("refresh: %v %v", err, resp)
	}
	resp.Body.Close()

	restarted, _ := NewOAuthServer(ts.URL, "secret")
	restarted.EnablePersistence(state, nil)
	if _, ok := restarted.refreshTokens[tokenKey(refresh)]; ok {
		t.Fatal("a rotated (spent) refresh token came back after a restart")
	}
}

func TestOAuthStateForAnotherOriginIsDiscarded(t *testing.T) {
	state := filepath.Join(t.TempDir(), "oauth.json")
	oauth, ts := oauthUnderTest(t, state, nil)
	_, access, _ := fullFlow(t, oauth, ts)

	moved, _ := NewOAuthServer("https://a-different-host.example", "secret")
	moved.EnablePersistence(state, nil)
	if moved.ValidateAccessToken(access) || len(moved.clients) != 0 {
		t.Fatal("state written for another origin must be discarded — moving hosts is a clean slate")
	}
}

func TestOAuthBadStateFileNeverStopsBoot(t *testing.T) {
	state := filepath.Join(t.TempDir(), "oauth.json")
	os.WriteFile(state, []byte("{not json"), 0o600)
	o, _ := NewOAuthServer("https://x.example", "secret")
	o.EnablePersistence(state, slog.New(slog.DiscardHandler)) // must not panic or error
	if len(o.clients) != 0 {
		t.Fatal("corrupt state should yield an empty server")
	}
	// and it keeps working: a save afterwards replaces the corrupt file
	o.clients["c"] = oauthClient{redirectURIs: []string{"https://x.example/cb"}}
	o.save()
	o2, _ := NewOAuthServer("https://x.example", "secret")
	o2.EnablePersistence(state, nil)
	if _, ok := o2.clients["c"]; !ok {
		t.Fatal("state should round-trip after the bad file was replaced")
	}
}

// The public Funnel exposes /authorize and /token to the internet, so no
// secret may ever reach a log line: run the whole flow with a wrong and a
// right credential, a refresh, and unauthenticated /mcp hits, capturing
// everything the server logs, and look for any token or the secret.
func TestNoSecretsReachTheLogs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	state := filepath.Join(t.TempDir(), "oauth.json")
	oauth, ts := oauthUnderTest(t, state, log)

	clientID, access, refresh := fullFlow(t, oauth, ts)
	authorizeAndGetCode(t, noRedirectClient(t), ts.URL, clientID, "https://client.example/callback", "c", "s", oauth.resource, "wrong-guess-XYZ")
	resp, _ := http.PostForm(ts.URL+"/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if resp != nil {
		resp.Body.Close()
	}
	for _, auth := range []string{"", "Bearer " + access, "Bearer bogus-token-ABC"} {
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
		}
	}

	logs := buf.String()
	for name, secret := range map[string]string{
		"the shared secret": "secret", "an access token": access, "a refresh token": refresh,
		"a wrong credential": "wrong-guess-XYZ", "a bogus bearer": "bogus-token-ABC",
	} {
		// "secret" is a substring of innocuous words; look for it as a value
		needle := secret
		if secret == "secret" {
			needle = "=secret"
		}
		if strings.Contains(logs, needle) {
			t.Errorf("%s appeared in the server's logs:\n%s", name, logs)
		}
	}
}
