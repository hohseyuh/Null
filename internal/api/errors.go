package api

import (
	"encoding/json"
	"net/http"
)

// writeJSON writes v as a JSON response with the given status. It assumes v
// marshals cleanly; a marshal failure is a programming error and yields a
// bare 500 without a body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(b)
}

// writeError writes the spec error shape: {"error": code, "detail": detail}.
// It assumes code is a stable machine-readable slug and detail is prose for
// a human (or an LLM) reading the response.
func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}
