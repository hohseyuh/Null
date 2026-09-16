package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"
)

// This file implements exactly enough of OAuth 2.1 + the MCP
// authorization spec's required RFCs (8414 authorization server
// metadata, 9728 protected resource metadata, 7591 dynamic client
// registration, 8707 resource indicators) for a single-user server to
// work with a spec-compliant MCP client — Claude.ai's connector UI,
// concretely, which mandates this over a plain bearer token.
//
// There is exactly one real credential in this whole flow: the same
// NULL_TOKEN this service already used as a static bearer secret. OAuth
// here is a protocol envelope around that one secret — asking for it
// once in a browser form and getting back a short-lived, audience-bound
// access token in exchange — not a user system. "No users, no roles,
// one human uses this" is still true; the wire protocol changed, the
// trust model didn't.
//
// Storage is in-memory and unauthenticated-by-design where the spec
// requires that (DCR, the metadata documents) — the credential check
// happens exactly once, at the authorize step, same as it always has.
// A restart drops all state; clients re-authenticate, which is normal
// OAuth client behavior on a 401.

const (
	authCodeTTL     = 5 * time.Minute
	accessTokenTTL  = 1 * time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour

	// maxRegisteredClients bounds memory from unauthenticated DCR spam.
	// Not a security control — registering doesn't grant access, only
	// the /authorize credential check does — just hygiene.
	maxRegisteredClients = 1000
)

type oauthClient struct {
	redirectURIs []string
}

type authCode struct {
	clientID      string
	redirectURI   string
	codeChallenge string
	resource      string
	expiresAt     time.Time
}

type issuedToken struct {
	clientID  string
	resource  string
	expiresAt time.Time
}

// OAuthServer is a minimal OAuth 2.1 authorization server plus resource
// server, scoped to protecting exactly one resource (this nullmcp
// instance's /mcp endpoint) for exactly one credential holder. baseURL
// must be the server's own public HTTPS origin (e.g.
// "https://host:10000") — every endpoint URL in the metadata documents
// is derived from it, and clients will fail discovery if it's wrong.
type OAuthServer struct {
	baseURL  string
	resource string // canonical resource URI: baseURL + HTTPPath
	secret   string // the shared credential — NULL_TOKEN
	tmpl     *template.Template

	mu            sync.Mutex
	clients       map[string]oauthClient
	codes         map[string]authCode
	accessTokens  map[string]issuedToken
	refreshTokens map[string]issuedToken
}

// NewOAuthServer builds an OAuthServer. baseURL must have no trailing
// slash (e.g. "https://host:10000", not ".../"). secret is the one
// credential the /authorize form checks — pass the same value used as
// the static bearer token, so there is exactly one secret to manage.
func NewOAuthServer(baseURL, secret string) (*OAuthServer, error) {
	tmpl, err := template.New("authorize").Parse(authorizeFormHTML)
	if err != nil {
		return nil, fmt.Errorf("parse authorize template: %w", err)
	}
	return &OAuthServer{
		baseURL:       baseURL,
		resource:      baseURL + HTTPPath,
		secret:        secret,
		tmpl:          tmpl,
		clients:       map[string]oauthClient{},
		codes:         map[string]authCode{},
		accessTokens:  map[string]issuedToken{},
		refreshTokens: map[string]issuedToken{},
	}, nil
}

// randomToken returns a 256-bit random token, hex-encoded. crypto/rand
// failing is treated as unrecoverable — there is no safe fallback for a
// security token generator.
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeOAuthError writes the {"error": ..., "error_description": ...}
// shape OAuth 2.1 error responses use (distinct from this codebase's
// usual {"error","detail"} shape — this one has to match what OAuth
// clients parse).
func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// ValidateAccessToken reports whether token is a currently-valid access
// token this server issued for its own resource. Expired tokens are
// evicted on lookup, so correctness never depends on the sweeper
// running.
func (o *OAuthServer) ValidateAccessToken(token string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	at, ok := o.accessTokens[token]
	if !ok {
		return false
	}
	if time.Now().After(at.expiresAt) {
		delete(o.accessTokens, token)
		return false
	}
	return at.resource == o.resource
}

// Sweep removes expired codes and tokens. Call periodically; correctness
// does not depend on it (every lookup path also checks expiry), this is
// purely to bound memory on a long-running process.
func (o *OAuthServer) Sweep() {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	for k, v := range o.codes {
		if now.After(v.expiresAt) {
			delete(o.codes, k)
		}
	}
	for k, v := range o.accessTokens {
		if now.After(v.expiresAt) {
			delete(o.accessTokens, k)
		}
	}
	for k, v := range o.refreshTokens {
		if now.After(v.expiresAt) {
			delete(o.refreshTokens, k)
		}
	}
}

// --- Discovery: RFC 9728 protected resource metadata ---

func (o *OAuthServer) protectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              o.resource,
		"authorization_servers": []string{o.baseURL},
	})
}

// --- Discovery: RFC 8414 authorization server metadata ---

func (o *OAuthServer) authServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                o.baseURL,
		"authorization_endpoint":                o.baseURL + "/authorize",
		"token_endpoint":                        o.baseURL + "/token",
		"registration_endpoint":                 o.baseURL + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// --- RFC 7591 dynamic client registration ---

func (o *OAuthServer) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "malformed JSON body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required and must be non-empty")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirectURI(u) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				fmt.Sprintf("redirect_uri %q must be https:// or an http:// localhost URL", u))
			return
		}
	}

	id := randomToken()
	o.mu.Lock()
	if len(o.clients) >= maxRegisteredClients {
		o.mu.Unlock()
		writeOAuthError(w, http.StatusTooManyRequests, "server_error", "too many registered clients")
		return
	}
	o.clients[id] = oauthClient{redirectURIs: req.RedirectURIs}
	o.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"client_name":                req.ClientName,
	})
}

// validRedirectURI enforces the spec's redirect URI rule directly:
// HTTPS always allowed, plain HTTP only for localhost. No fragment
// (fragments are stripped by user agents before the redirect reaches
// the client anyway, so one present here indicates a malformed value).
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	default:
		return false
	}
}

// --- /authorize: GET shows the credential form, POST checks it ---

type authorizeFormData struct {
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
	Resource      string
	Error         string
}

const authorizeFormHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Authorize</title>
<style>
  body { background:#14161a; color:#c9cdd3; font:15px/1.55 ui-sans-serif, system-ui, sans-serif;
         max-width: 32rem; margin: 4rem auto; padding: 0 1rem; }
  h1 { font-size: 1.1rem; color:#e4e7ec; }
  p.sub { color:#8a919c; font-size:0.88rem; }
  input[type=password] { width:100%; font: inherit; background:#1c1f25; color:#c9cdd3;
    border:1px solid #2a2e36; border-radius:4px; padding:0.6rem; margin-top:0.4rem; }
  button { font: inherit; background:#1c1f25; color:#c9cdd3; border:1px solid #2a2e36;
    border-radius:4px; padding:0.5rem 1.2rem; margin-top:1rem; cursor:pointer; }
  button:hover { border-color:#4a6191; }
  .err { color:#b3616a; margin-top:0.6rem; }
</style>
</head>
<body>
<h1>Authorize access to this vault</h1>
<p class="sub">A client is requesting access. Enter the server token to approve.</p>
<form method="post">
  <input type="hidden" name="client_id" value="{{.ClientID}}">
  <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
  <input type="hidden" name="state" value="{{.State}}">
  <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
  <input type="hidden" name="resource" value="{{.Resource}}">
  <input type="password" name="token" placeholder="server token" autofocus required>
  <button type="submit">Authorize</button>
</form>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
</body>
</html>
`

func (o *OAuthServer) authorizeGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data, ok := o.validateAuthorizeParams(w, r, q.Get("client_id"), q.Get("redirect_uri"),
		q.Get("response_type"), q.Get("code_challenge"), q.Get("code_challenge_method"),
		q.Get("state"), q.Get("resource"))
	if !ok {
		return
	}
	o.renderAuthorizeForm(w, data)
}

func (o *OAuthServer) authorizePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	data, ok := o.validateAuthorizeParams(w, r, f.Get("client_id"), f.Get("redirect_uri"),
		"code", f.Get("code_challenge"), "S256", f.Get("state"), f.Get("resource"))
	if !ok {
		return
	}

	given := f.Get("token")
	if subtle.ConstantTimeCompare([]byte(given), []byte(o.secret)) != 1 {
		data.Error = "Incorrect token."
		o.renderAuthorizeForm(w, data)
		return
	}

	code := randomToken()
	o.mu.Lock()
	o.codes[code] = authCode{
		clientID: data.ClientID, redirectURI: data.RedirectURI,
		codeChallenge: data.CodeChallenge, resource: data.Resource,
		expiresAt: time.Now().Add(authCodeTTL),
	}
	o.mu.Unlock()

	dest, _ := url.Parse(data.RedirectURI)
	qs := dest.Query()
	qs.Set("code", code)
	if data.State != "" {
		qs.Set("state", data.State)
	}
	dest.RawQuery = qs.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

// validateAuthorizeParams checks everything that must be verified
// before a token is ever requested from the user: known client,
// registered redirect_uri (exact match — the open-redirect guard),
// response_type, PKCE presence, and resource audience. A redirect_uri
// mismatch or unknown client fails closed, in place, rather than
// redirecting anywhere — redirecting on that specific failure is
// exactly the open-redirect vulnerability the spec warns about.
func (o *OAuthServer) validateAuthorizeParams(
	w http.ResponseWriter, r *http.Request,
	clientID, redirectURI, responseType, codeChallenge, codeChallengeMethod, state, resource string,
) (authorizeFormData, bool) {
	o.mu.Lock()
	client, known := o.clients[clientID]
	o.mu.Unlock()
	if !known {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return authorizeFormData{}, false
	}
	if redirectURI == "" || !slices.Contains(client.redirectURIs, redirectURI) {
		http.Error(w, "redirect_uri does not match a registered value", http.StatusBadRequest)
		return authorizeFormData{}, false
	}
	data := authorizeFormData{
		ClientID: clientID, RedirectURI: redirectURI, State: state,
		CodeChallenge: codeChallenge, Resource: resource,
	}
	if responseType != "code" {
		o.redirectError(w, r, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return data, false
	}
	if codeChallenge == "" || codeChallengeMethod != "S256" {
		o.redirectError(w, r, redirectURI, state, "invalid_request", "PKCE (code_challenge with S256) is required")
		return data, false
	}
	if resource != o.resource {
		o.redirectError(w, r, redirectURI, state, "invalid_target", fmt.Sprintf("resource must be %q", o.resource))
		return data, false
	}
	return data, true
}

// redirectError sends the user back to the client's redirect_uri with
// error/error_description query params, per the spec — used only once
// redirect_uri itself has already been validated as registered.
func (o *OAuthServer) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	dest, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	qs := dest.Query()
	qs.Set("error", code)
	qs.Set("error_description", desc)
	if state != "" {
		qs.Set("state", state)
	}
	dest.RawQuery = qs.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

func (o *OAuthServer) renderAuthorizeForm(w http.ResponseWriter, data authorizeFormData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	o.tmpl.Execute(w, data)
}

// --- /token ---

func (o *OAuthServer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		o.tokenFromCode(w, r)
	case "refresh_token":
		o.tokenFromRefresh(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"grant_type must be authorization_code or refresh_token")
	}
}

func (o *OAuthServer) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	code := f.Get("code")

	o.mu.Lock()
	ac, ok := o.codes[code]
	if ok {
		delete(o.codes, code) // single-use, unconditionally, even on a later mismatch below
	}
	o.mu.Unlock()

	if !ok || time.Now().After(ac.expiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown or expired code")
		return
	}
	if f.Get("client_id") != ac.clientID || f.Get("redirect_uri") != ac.redirectURI || f.Get("resource") != ac.resource {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id, redirect_uri, or resource does not match the authorization request")
		return
	}
	if !pkceMatches(f.Get("code_verifier"), ac.codeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match code_challenge")
		return
	}

	o.issueTokenPair(w, ac.clientID, ac.resource)
}

func (o *OAuthServer) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	rt := f.Get("refresh_token")

	o.mu.Lock()
	old, ok := o.refreshTokens[rt]
	if ok {
		delete(o.refreshTokens, rt) // rotation: this refresh token is spent regardless of outcome
	}
	o.mu.Unlock()

	if !ok || time.Now().After(old.expiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown or expired refresh_token")
		return
	}
	if res := f.Get("resource"); res != "" && res != old.resource {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", fmt.Sprintf("resource must be %q", old.resource))
		return
	}

	o.issueTokenPair(w, old.clientID, old.resource)
}

func (o *OAuthServer) issueTokenPair(w http.ResponseWriter, clientID, resource string) {
	access := randomToken()
	refresh := randomToken()
	now := time.Now()

	o.mu.Lock()
	o.accessTokens[access] = issuedToken{clientID: clientID, resource: resource, expiresAt: now.Add(accessTokenTTL)}
	o.refreshTokens[refresh] = issuedToken{clientID: clientID, resource: resource, expiresAt: now.Add(refreshTokenTTL)}
	o.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
	})
}

// pkceMatches recomputes BASE64URL(SHA256(verifier)) and compares it to
// challenge, constant-time. S256 is the only method this server accepts
// (enforced at /authorize); "plain" is deliberately not supported.
func pkceMatches(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}
