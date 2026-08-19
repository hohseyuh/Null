package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireBearer rejects any request whose Authorization header is not
// exactly "Bearer <token>". Every route is gated — including unknown paths,
// so the router leaks nothing to the unauthenticated — except /v1/health,
// which is exempt by spec. Comparison is constant-time over SHA-256
// digests so neither content nor length of the configured token leaks
// through timing. It assumes s.Token is non-empty (enforced at boot).
func (s *Server) requireBearer(next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(s.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		got := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
