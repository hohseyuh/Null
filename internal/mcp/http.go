package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPPath is the single path the Streamable HTTP transport is mounted
// at. Not a protocol requirement — MCP over HTTP has no fixed convention
// — chosen so the URL a client is given ("https://host/mcp") reads
// plainly.
const HTTPPath = "/mcp"

// NewHTTPHandler wraps server in the MCP Streamable HTTP transport,
// mounted at HTTPPath and gated by a bearer token on every request —
// this transport carries no authentication of its own, so the token is
// the entire reason it is safe to put behind a public reverse proxy at
// all. Comparison is constant-time over SHA-256 digests, the same
// discipline internal/api/auth.go uses for the JSON API. token must be
// non-empty; callers enforce that before calling this (see
// cmd/nullmcp's config validation).
//
// DisableLocalhostProtection is set deliberately: this handler is meant
// to sit behind a local reverse proxy (Tailscale Funnel, in nullmcp's
// own deployment) that connects to it via loopback while forwarding the
// original public Host header — which the SDK's default DNS-rebinding
// check would otherwise reject outright. The bearer token is the real
// boundary here; disabling that unrelated check does not weaken it.
func NewHTTPHandler(server *sdkmcp.Server, token string, log *slog.Logger) http.Handler {
	mcpHandler := sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return server },
		&sdkmcp.StreamableHTTPOptions{
			Logger:                     log,
			DisableLocalhostProtection: true,
		},
	)

	mux := http.NewServeMux()
	mux.Handle(HTTPPath, requireBearerHTTP(token, mcpHandler))
	// Unauthenticated, deliberately: liveness only, no vault content, no
	// tool schemas, nothing an operator's monitoring shouldn't see.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return mux
}

// requireBearerHTTP rejects any request whose Authorization header is
// not exactly "Bearer <token>". Mirrors internal/api/auth.go's
// requireBearer; not shared code because internal/mcp deliberately
// carries no dependency on internal/api (see tools.go's package doc).
func requireBearerHTTP(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		got, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		gotHash := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(gotHash[:], want[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
