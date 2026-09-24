package render

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"null-service/internal/session"
	"null-service/internal/vault"
)

// handleMina serves GET /al-mina: the port. Basim's proposals wait here
// and the user approves, denies, or defers them in one batched pass. It is
// a view over the same MinaQueue that backs GET /mina — not a tier, and
// not storage; the proposals live in each note's own frontmatter.
func (rd *Renderer) handleMina(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	done, _ := strconv.Atoi(q.Get("done"))
	failed, _ := strconv.Atoi(q.Get("failed"))
	rd.render(w, http.StatusOK, "mina.html", map[string]any{
		"Title":    "Al-Mina",
		"Entries":  vault.MinaQueue(rd.Index, vault.MinaStaleDays(), time.Now()),
		"CSRF":     rd.csrf,
		"Writable": rd.Writable,
		"Done":     done,
		"Failed":   failed,
	})
}

// handleMinaAct serves POST /al-mina/act: every decision the user made on
// the screen, applied one note at a time — each its own git commit.
// Form fields are act:<path> = approve|deny|defer (anything else, or an
// absent field, leaves that note untouched) and reason:<path> for a
// denial's explanation. This is the only HTTP route that raises a tier.
func (rd *Renderer) handleMinaAct(w http.ResponseWriter, r *http.Request) {
	if !rd.Writable {
		http.Error(w, "vault is not writable (is it a git repository?)", http.StatusServiceUnavailable)
		return
	}
	// Same-origin AND the derived token: a form injected into a note body
	// is same-origin but cannot know the token.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !session.SameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(rd.csrf)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	done, failed := 0, 0
	for key, vals := range r.PostForm {
		p, ok := strings.CutPrefix(key, "act:")
		if !ok || len(vals) == 0 {
			continue
		}
		action := vals[0]
		if action != "approve" && action != "deny" && action != "defer" {
			continue // "skip" or anything unknown: leave the note alone
		}
		rel, err := vault.SafeRequestPath(rd.VaultRoot, p)
		if err != nil {
			failed++
			continue
		}
		switch action {
		case "approve":
			err = vault.ApproveProposal(rd.VaultRoot, rel)
		case "deny":
			err = vault.DenyProposal(rd.VaultRoot, rel, strings.TrimSpace(r.PostForm.Get("reason:"+p)))
		case "defer":
			err = vault.DeferProposal(rd.VaultRoot, rel)
		}
		if err != nil {
			if !errors.Is(err, vault.ErrNoProposal) { // a stale page is not worth a log line
				rd.Log.Error("al-mina action", "action", action, "path", rel, "err", err)
			}
			failed++
			continue
		}
		rd.Index.Refresh(rel)
		done++
	}
	http.Redirect(w, r, "/al-mina?done="+strconv.Itoa(done)+"&failed="+strconv.Itoa(failed), http.StatusSeeOther)
}

// handleGraph serves GET /graph: the whole vault as a force-directed
// graph, drawn by the one script this build ships (static/graph.js).
func (rd *Renderer) handleGraph(w http.ResponseWriter, _ *http.Request) {
	rd.render(w, http.StatusOK, "graph.html", map[string]any{"Title": "Graph"})
}

// handleGraphData serves GET /graph/data: every note and every distinct
// resolved link, compact JSON. Nothing is excluded by tier; each node
// carries its tier and each edge the lower of its endpoints'.
func (rd *Renderer) handleGraphData(w http.ResponseWriter, _ *http.Request) {
	nodes, edges := rd.Index.WholeGraph()
	b, err := json.Marshal(map[string]any{"nodes": nodes, "edges": edges})
	if err != nil {
		http.Error(w, "graph failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(b)
}
