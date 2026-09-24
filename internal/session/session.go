// Package session is the browser-facing half of authentication: the
// single static bearer token, checked constant-time from either an
// Authorization header or a cookie, plus the one-shot /login route that
// sets the cookie. It exists so the renderer and the setup page share one
// implementation instead of two copies of a security-sensitive compare.
//
// It also records HOW a request authenticated. The renderer counts only
// human reads — cookie sessions — toward promoting a note from dakhil to
// amil; a request carrying the Authorization header is a program (the
// JSON API's client, a model) and must never promote anything.
package session

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
)

// CookieName holds the bearer token for browsers; set by GET /login.
const CookieName = "null_token"

type ctxKey struct{}

// Guard checks requests against one configured token.
type Guard struct {
	hash [32]byte
}

// NewGuard returns a Guard for token. It assumes token is non-empty;
// callers refuse to boot without one.
func NewGuard(token string) *Guard {
	return &Guard{hash: sha256.Sum256([]byte(token))}
}

// Valid reports whether candidate equals the token, in constant time
// (both sides are hashed first so lengths never leak).
func (g *Guard) Valid(candidate string) bool {
	got := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(got[:], g.hash[:]) == 1
}

// Require wraps next, letting through requests whose bearer header or
// cookie matches the token and answering everything else with a 401 page.
// The header is checked first; a request with a header is never treated
// as a cookie session even if it also carries a valid cookie.
func (g *Guard) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidate, viaCookie := "", false
		if h, ok := cutBearer(r.Header.Get("Authorization")); ok {
			candidate = h
		} else if c, err := r.Cookie(CookieName); err == nil {
			candidate, viaCookie = c.Value, true
		}
		if !g.Valid(candidate) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>401</title>`+
				`<body style="background:#14161a;color:#c9cdd3;font:16px/1.6 system-ui;padding:4rem">`+
				`<p>Unauthorized. Visit <code>/login?token=&lt;token&gt;</code> once; a cookie will keep you in.</p>`)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, viaCookie)))
	})
}

func cutBearer(h string) (string, bool) {
	const p = "Bearer "
	if len(h) >= len(p) && h[:len(p)] == p {
		return h[len(p):], true
	}
	return "", false
}

// ViaCookie reports whether the request was authenticated by the browser
// cookie — a human session — rather than by an Authorization header.
// False for any request that did not pass through Require.
func ViaCookie(r *http.Request) bool {
	v, _ := r.Context().Value(ctxKey{}).(bool)
	return v
}

// Login sets the token cookie so a browser can hold the bearer token.
// Deliberately not a form — the token travels in the query string exactly
// once, over TLS in deployment.
func (g *Guard) Login(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if !g.Valid(token) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// SameOrigin reports whether a state-changing request plainly came from
// this site's own pages. Browsers send Sec-Fetch-Site on every request;
// "same-origin" (or "none" for a user-typed navigation) passes. When the
// header is absent — curl, older browsers — it falls back to comparing
// Origin's host with Host, and a request with neither is allowed only
// because it cannot be a cross-site browser form post (those always carry
// one of the two). Used to guard the POST routes that write to the
// vault, on top of SameSite=Lax cookies.
func SameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}
