# CLAUDE.md — Null

Standing context for every session in this repo. Short on purpose: it holds the
rules and the map. The detail lives in `docs/` and `spec/` — read what your
task touches, not everything.

## What this is

Null serves a **folder of markdown files (the vault, kept under git)** three
ways from one in-memory index: a **JSON API** (for programs), a **web reader**
(for the human owner — graph view, review screen, setup page), and an **MCP
server** (for a language model, which can also *write* notes). This repo is the
service, not the vault: it contains no notes except the fixture under
`testdata/vault/`. Files are the source of truth; delete the server and nothing
is lost.

The organising idea: **the model is not trusted.** Every note has a server-owned
curation *tier* (`dakhil` → `amil` → `thabit` → `asil`, meaning how far a human
has reviewed it). The model can lower a tier or *propose* a raise; only the human
raises one. Those rules are code that returns errors, never instructions to the
model. "Basim" is the owner's planned personal assistant — the model that will
call the MCP tools; he does not exist yet, so don't build for him speculatively.

## Where to read

| You need | Read |
|---|---|
| How it works, who can do what, routes, env vars, deployment, which test enforces which rule | [`docs/architecture.md`](docs/architecture.md) |
| Why it is this way, and every deliberate reversal (so you don't undo one) | [`docs/decisions.md`](docs/decisions.md) |
| Tier semantics and the permission matrix (canonical) | [`spec/tiers.md`](spec/tiers.md) |
| HTTP API contract | [`spec/null-read-api-v0.md`](spec/null-read-api-v0.md) |
| MCP tools, OAuth, write model | [`spec/null-mcp-v0.md`](spec/null-mcp-v0.md) |
| Running it / user-facing setup | [`README.md`](README.md) |
| Security model and reporting | [`SECURITY.md`](SECURITY.md) |
| The original read-only build plan (**archived, stale**) | [`docs/history/build-plan-v0.md`](docs/history/build-plan-v0.md) |

## Non-negotiables

Architectural commitments, not preferences. If a change would violate one, stop
and say so instead of working around it. Reasons are in `docs/decisions.md`.

1. **Files are canonical.** No database of record, no proprietary index, no note that exists only in the server.
2. **The JSON API and the indexer never write to the vault.** Exactly two other things do: `nullmcp` (note content, permission-checked) and the renderer's two *human* tier actions (`internal/vault/human.go`). Every write is exactly **one git commit for one note**, never batched — that, not a staging area, is what keeps a bad write cheap to undo. Consequently the vault mount is read-write and the vault must be a git repo.
3. **Never return a body that wasn't explicitly requested.** `/notes` and `/search` return metadata and snippets; bodies come only from `GET /notes/{path}` / `get_note`. This keeps a large vault out of an LLM's context window.
4. **Path is the identity.** No UUIDs. Every request-supplied path goes through `vault.SafeRequestPath`; the setup folder picker goes through `setup.Browser.Resolve`. Never hand-roll a path check.
5. **Never touch dotfiles or dot-directories** (`.git/` above all): excluded from indexing, unreachable via any route.
6. **The renderer shares the index.** It does not call the API over HTTP or keep a second parse.
7. **The model never raises a tier and never pushes or commits.** The permission matrix, R1 (no raising), R2 (editing `thabit` demotes to `amil` atomically), the `asil` lock (also `0444` on disk) and stripping of server-owned frontmatter (`tier`, `proposed_*`, `denied_*`, `tier_history`) are enforced in `internal/vault`. No MCP tool exposes git commit/push or raw filesystem writes (`TestNoToolExposesGitOrFilesystemWrite`). Tier-raising code lives only in `internal/vault/human.go` and may be referenced only from `internal/render` (`TestOnlyTheRendererCanRaiseTiers`).
8. **The renderer treats every note body as hostile** (a model authors them; they render as raw HTML; the renderer writes). Keep the strict CSP, the CSRF token on write forms, the same-origin check on POSTs, and promotion-on-first-open counting only a genuine user navigation. Do not weaken any of these to ease a UI feature.
9. **Keep the API and UI credentials separable** (`NULL_TOKEN` vs `NULL_UI_TOKEN`), and keep the UI's `NULL_BROWSE_ROOT` confinement intact.

## Layout

```
cmd/nullapi/            entrypoint: env settings → app.Manager
cmd/nullmcp/            MCP entrypoint (stdio, or Streamable HTTP + OAuth)
internal/vault/         the core — no HTTP
  note.go tier.go       Note/frontmatter/headings/wikilinks; Tier type, ranking, weights, server-owned keys
  index.go watcher.go   in-memory index + backlinks, fsnotify, asil 0444 lock
  path.go               SafeRequestPath — the one path gate
  write.go              the MODEL's writes: Create/Write/Delete/SetTier/ProposeTier, each one git commit
  human.go              the HUMAN's tier writes: MarkOpened/Approve/Deny/DeferProposal (render-only)
  mina.go graph.go      Al-Mina queue, tier counts, whole-vault graph, BFS neighbourhood
internal/api/           JSON API handlers (read-only), bearer auth
internal/render/        web reader: templates, goldmark→HTML, Al-Mina, graph view, CSP/CSRF; static/{style.css,graph.js,mina.js}
internal/session/       token check, login cookie, same-origin check (shared by render + setup)
internal/setup/         /setup: confined folder browser + activate handler
internal/config/        the saved vault choice (atomic, 0600 JSON file)
internal/app/           Manager: builds index/watcher/API/renderer for a vault and hot-swaps it; setup mode
internal/mcp/           MCP tools (tools.go = logic, no SDK import), server.go, http.go, oauth.go, inspector.go
internal/search/        ripgrep wrapper, diacritic folding
spec/                   contracts (see table above)     docs/  architecture, decisions, history
testdata/vault/         the only notes in this repo: unicode note, frontmatter-less note, broken wikilink, a hidden dir
```

## Stack

Go (version in `go.mod`), stdlib `net/http` + `chi`; `goldmark`, `goccy/go-yaml`;
`ripgrep` and `git` **shelled out** (not libraries); official MCP Go SDK;
`fsnotify`. Server-rendered `html/template`, no framework, no build step; JS is
limited to two hand-written embedded files (`graph.js`, `mina.js`). No database.
No TLS in the binaries — a reverse proxy terminates it.

## Conventions

- Errors as values, wrapped with `%w`; no panics outside `main`.
- Handlers do HTTP only; logic lives in `internal/vault` / `internal/search` and is testable without a server.
- Every exported func has a doc comment saying what it does **and what it assumes**.
- Table-driven tests over the fixture vault. **Never run write paths against `testdata/vault/`** — copy it into a temp git repo (see the `gitTempRepo`/`gitVaultTools`/`writableFixture` helpers).
- Wire format is compact JSON (`json.Marshal`, never `MarshalIndent`).
- No dependency without saying why in the commit message. Concise, imperative commits; no emoji.
- Before finishing: `gofmt -l .` empty, `go vet ./...`, `go test ./... -race`.

## Working style

- Answer first, then reason. No preamble.
- If a spec is ambiguous, say so and pick the simpler reading — don't implement both.
- Flag scope creep before starting, not after.
- Don't add anything from "Deliberately absent" however easy it looks.
- When you disagree with a decision here, say it once with the reason, then implement what was asked.
- Test the deployment shape you change, not just unit tests (a fixed-vault compose file once broke because only the setup layout had been run).

## Deliberately absent

Writes through the JSON API · any renderer write other than the two human tier
actions · in-browser editing of note bodies · embeddings/RAG · users, roles,
sharing · rate limiting · a JS build step, or JS beyond `graph.js`/`mina.js` · a
push/commit tool for the model · anything that lets a browser session name a
path outside `NULL_BROWSE_ROOT` · pagination beyond a cursor.

Each is a real future need; none is specifiable before the vault has been used
for a while. The renderer is a **reader** with one narrow exception (a human
raising a tier or recording a denial); a text box for note bodies is the line —
a general editor is years of polish for something the owner's editor already does.

## Open items

`epistemics.md` (the `asil` note in the *vault* that teaches a model to read the
tiers) is not written; the remote deployment predates direct writes and tiers and
needs redeploying. See "Status and open items" in `docs/architecture.md`.
