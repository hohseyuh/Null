package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPPath is the single path the Streamable HTTP transport is mounted
// at. Not a protocol requirement — MCP over HTTP has no fixed convention
// — chosen so the URL a client is given ("https://host/mcp") reads
// plainly. It also doubles as the OAuth resource identifier
// (OAuthServer.resource = baseURL + HTTPPath), so changing it changes
// what "this resource" means to already-registered clients.
const HTTPPath = "/mcp"

// NewHTTPHandler wraps server in the MCP Streamable HTTP transport,
// mounted at HTTPPath, plus the OAuth 2.1 endpoints oauth implements
// (discovery, dynamic client registration, authorize, token) — see
// oauth.go's package doc for why this exists at all: the MCP
// authorization spec, which Claude.ai's connector UI requires, expects a
// real OAuth flow, not a static header.
//
// /mcp accepts either the raw static token (equal to oauth's own
// secret — useful for curl/testing and any client that doesn't want the
// OAuth dance) or a valid access token oauth issued. Either is checked
// constant-time; neither is weaker than the other, since both ultimately
// trace back to the same one credential. A missing or invalid token gets
// a WWW-Authenticate header naming the protected-resource metadata URL,
// per RFC 9728 — this is what lets a spec-compliant client discover the
// OAuth flow in the first place instead of just failing.
//
// DisableLocalhostProtection is set deliberately: this handler is meant
// to sit behind a local reverse proxy (Tailscale Funnel, in nullmcp's
// own deployment) that connects via loopback while forwarding the
// original public Host header — which the SDK's default DNS-rebinding
// check would otherwise reject outright. The bearer check is the real
// boundary here; disabling that unrelated check does not weaken it.
func NewHTTPHandler(server *sdkmcp.Server, oauth *OAuthServer, staticToken string, log *slog.Logger) http.Handler {
	mcpHandler := sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return server },
		&sdkmcp.StreamableHTTPOptions{
			Logger:                     log,
			DisableLocalhostProtection: true,
		},
	)

	mux := http.NewServeMux()
	mux.Handle(HTTPPath, requireBearerHTTP(oauth, staticToken, mcpHandler))

	mux.HandleFunc("GET /.well-known/oauth-protected-resource", oauth.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", oauth.authServerMetadata)
	mux.HandleFunc("POST /register", oauth.register)
	mux.HandleFunc("GET /authorize", oauth.authorizeGet)
	mux.HandleFunc("POST /authorize", oauth.authorizePost)
	mux.HandleFunc("POST /token", oauth.token)

	// Unauthenticated, deliberately: liveness only, no vault content, no
	// tool schemas, nothing an operator's monitoring shouldn't see.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return mux
}

// requireBearerHTTP accepts the raw static token or a valid OAuth
// access token oauth issued, constant-time either way. On rejection it
// sets WWW-Authenticate per RFC 9728 §5.1 so a spec-compliant client can
// discover the OAuth flow instead of just seeing an opaque 401.
func requireBearerHTTP(oauth *OAuthServer, staticToken string, next http.Handler) http.Handler {
	wantStatic := sha256.Sum256([]byte(staticToken))

	// challenge is computed per-request from oauth.baseURL rather than
	// captured once here, so this handler's correctness never depends on
	// construction order relative to when baseURL was finalized.
	challenge := func() string {
		return fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, oauth.baseURL)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", challenge())
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		gotHash := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(gotHash[:], wantStatic[:]) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if oauth.ValidateAccessToken(got) {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("WWW-Authenticate", challenge())
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}
