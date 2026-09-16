package mcp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// pkcePair returns a matching (verifier, S256 challenge) pair.
func pkcePair() (verifier, challenge string) {
	verifier = randomToken() // any high-entropy string works as a verifier
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

// register performs dynamic client registration and returns the issued
// client_id.
func register(t *testing.T, baseURL string, redirectURIs ...string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"redirect_uris": redirectURIs, "client_name": "test"})
	resp, err := http.Post(baseURL+"/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status = %d", resp.StatusCode)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ClientID == "" {
		t.Fatal("register: empty client_id")
	}
	return out.ClientID
}

// authorize drives the /authorize GET+POST steps with the given
// credential and returns the authorization code from the final
// redirect, or ("", redirectURL) if the server redirected with an error
// instead of a code.
func authorizeAndGetCode(t *testing.T, client *http.Client, baseURL, clientID, redirectURI, challenge, state, resource, credential string) (code string, finalURL *url.URL) {
	t.Helper()
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"state": {state}, "resource": {resource},
	}
	getResp, err := client.Get(baseURL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("authorize GET: status = %d", getResp.StatusCode)
	}

	form := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirectURI}, "state": {state},
		"code_challenge": {challenge}, "resource": {resource}, "token": {credential},
	}
	postResp, err := client.PostForm(baseURL+"/authorize", form)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()

	// client doesn't follow redirects (see noRedirectClient). A wrong
	// credential re-renders the form (200, no redirect) rather than
	// redirecting with an error — the caller checks for an empty code
	// either way, so that's signaled by returning a nil final URL here.
	loc := postResp.Header.Get("Location")
	if loc == "" {
		return "", nil
	}
	finalURL, err = url.Parse(loc)
	if err != nil {
		t.Fatalf("bad Location header %q: %v", loc, err)
	}
	return finalURL.Query().Get("code"), finalURL
}

// noRedirectClient never follows redirects, so the test can inspect the
// Location header of the authorize POST directly instead of the page
// the browser would land on.
func noRedirectClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestOAuthFullFlow(t *testing.T) {
	srv := testServer(t)
	oauth, err := NewOAuthServer("http://placeholder", "secret")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewHTTPHandler(srv, oauth, "secret", nil))
	t.Cleanup(ts.Close)
	oauth.baseURL, oauth.resource = ts.URL, ts.URL+HTTPPath

	// discovery documents
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	mustGetJSON(t, ts.URL+"/.well-known/oauth-protected-resource", &prm)
	if prm.Resource != oauth.resource || len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != oauth.baseURL {
		t.Fatalf("protected resource metadata = %+v", prm)
	}
	var asm struct {
		TokenEndpoint        string   `json:"token_endpoint"`
		RegistrationEndpoint string   `json:"registration_endpoint"`
		CodeChallengeMethods []string `json:"code_challenge_methods_supported"`
	}
	mustGetJSON(t, ts.URL+"/.well-known/oauth-authorization-server", &asm)
	if asm.TokenEndpoint != oauth.baseURL+"/token" || len(asm.CodeChallengeMethods) != 1 || asm.CodeChallengeMethods[0] != "S256" {
		t.Fatalf("auth server metadata = %+v", asm)
	}

	clientID := register(t, ts.URL, "https://client.example/callback")
	verifier, challenge := pkcePair()

	// wrong credential at /authorize: re-shown the form, no code issued
	client := noRedirectClient(t)
	code, _ := authorizeAndGetCode(t, client, ts.URL, clientID, "https://client.example/callback", challenge, "xyz", oauth.resource, "wrong-token")
	if code != "" {
		t.Fatal("expected no code for a wrong credential")
	}

	// correct credential: redirected with a code and the original state
	code, finalURL := authorizeAndGetCode(t, client, ts.URL, clientID, "https://client.example/callback", challenge, "xyz", oauth.resource, "secret")
	if code == "" {
		t.Fatalf("expected a code, got redirect to %s", finalURL)
	}
	if finalURL.Query().Get("state") != "xyz" {
		t.Fatalf("state not echoed back: %s", finalURL)
	}

	// code exchange with the WRONG verifier fails
	badForm := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {"https://client.example/callback"}, "resource": {oauth.resource},
		"code_verifier": {"not-the-real-verifier"},
	}
	resp, err := http.PostForm(ts.URL+"/token", badForm)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong verifier: status = %d, want 400", resp.StatusCode)
	}

	// a code is single-use: even after that failed attempt the SAME code
	// must not work with the right verifier either — get a fresh one
	code, _ = authorizeAndGetCode(t, client, ts.URL, clientID, "https://client.example/callback", challenge, "xyz", oauth.resource, "secret")

	// correct exchange succeeds
	goodForm := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {"https://client.example/callback"}, "resource": {oauth.resource},
		"code_verifier": {verifier},
	}
	resp, err = http.PostForm(ts.URL+"/token", goodForm)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token exchange: status = %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.TokenType != "Bearer" {
		t.Fatalf("token response = %+v", tok)
	}

	// re-using the SAME code again fails — single use
	resp, err = http.PostForm(ts.URL+"/token", goodForm)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("code replay: status = %d, want 400", resp.StatusCode)
	}

	// the issued access token actually works against /mcp
	req, _ := http.NewRequest(http.MethodPost, ts.URL+HTTPPath, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	mcpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mcpResp.Body.Close()
	if mcpResp.StatusCode == http.StatusUnauthorized {
		t.Fatal("issued access token was rejected at /mcp")
	}

	// refresh rotates: old refresh token stops working, new one works
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}
	resp, err = http.PostForm(ts.URL+"/token", refreshForm)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: status = %d", resp.StatusCode)
	}
	var tok2 struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	json.NewDecoder(resp.Body).Decode(&tok2)
	if tok2.AccessToken == tok.AccessToken || tok2.RefreshToken == tok.RefreshToken {
		t.Fatal("refresh did not rotate tokens")
	}

	resp, err = http.PostForm(ts.URL+"/token", refreshForm) // old refresh token, again
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reused (rotated-away) refresh token: status = %d, want 400", resp.StatusCode)
	}
}

func TestOAuthRedirectURIMismatchFailsClosed(t *testing.T) {
	srv := testServer(t)
	oauth, err := NewOAuthServer("http://placeholder", "secret")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewHTTPHandler(srv, oauth, "secret", nil))
	t.Cleanup(ts.Close)
	oauth.baseURL, oauth.resource = ts.URL, ts.URL+HTTPPath

	clientID := register(t, ts.URL, "https://client.example/callback")
	_, challenge := pkcePair()

	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {"https://evil.example/steal"}, // not registered
		"response_type": {"code"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"resource": {oauth.resource},
	}
	resp, err := http.Get(ts.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Must fail IN PLACE with a 4xx, never redirect anywhere — redirecting
	// here is exactly the open-redirect vulnerability the spec warns about.
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("unregistered redirect_uri: status = %d, want 4xx", resp.StatusCode)
	}
}

func TestOAuthRequiresPKCE(t *testing.T) {
	srv := testServer(t)
	oauth, err := NewOAuthServer("http://placeholder", "secret")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewHTTPHandler(srv, oauth, "secret", nil))
	t.Cleanup(ts.Close)
	oauth.baseURL, oauth.resource = ts.URL, ts.URL+HTTPPath

	clientID := register(t, ts.URL, "https://client.example/callback")

	client := noRedirectClient(t)
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {"https://client.example/callback"},
		"response_type": {"code"}, "resource": {oauth.resource}, // no code_challenge at all
	}
	resp, err := client.Get(ts.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "error=invalid_request") {
		t.Fatalf("missing PKCE should redirect with an error, got status=%d location=%q", resp.StatusCode, loc)
	}
}

func TestOAuthRegisterValidatesRedirectURIs(t *testing.T) {
	srv := testServer(t)
	oauth, err := NewOAuthServer("http://placeholder", "secret")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewHTTPHandler(srv, oauth, "secret", nil))
	t.Cleanup(ts.Close)

	tests := []struct {
		name string
		uris []any
	}{
		{"empty", []any{}},
		{"plain http non-localhost", []any{"http://evil.example/callback"}},
		{"not a url", []any{"not-a-url"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"redirect_uris": tt.uris})
			resp, err := http.Post(ts.URL+"/register", "application/json", strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func mustGetJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}
