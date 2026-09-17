# CLAUDE.md — Null

A markdown vault served three ways: a read-only JSON API, a read-only HTML renderer, and an MCP layer that also reads and writes the vault directly, one git commit per note. This file is the standing context for every session in this repo.

---

## What this is

`Null` is a plain-markdown knowledge vault living on a VPS, under git. This repo is **not** the vault — it is a service that indexes the vault and presents it three ways: a read-only JSON API (`nullapi`), a read-only HTML renderer (same process as `nullapi`), and an MCP layer (`nullmcp`) that also writes — see "Writes" below.

**No Obsidian.** No GUI editor is part of this system. Two write paths now: a human authors locally in whatever editor they prefer and reaches the VPS by `git push` (the server pulls or watches the working tree); an LLM through `nullmcp`'s tools writes directly to the VPS's own clone and commits there. Either way the vault stays plain files under git — nothing here invents a second source of truth.

Files are the source of truth. This server is a lens over them and holds no state that would survive its own deletion.

## Two repos

**`null-vault`** — the notes. Nothing but `.md` and directories. Private, backed up on its own cadence, no CI, no code.

**`null-service`** — this repo. The Go server. Contains no notes, ever, except the fixture vault under `testdata/`.

**They connect by bind mount, not by submodule.** The vault is cloned to a path on the VPS; each process mounts it (`nullapi` read-only, `nullmcp` read-write) and is pointed at it by `NULL_VAULT_PATH`. Sync from a human's own machine is still a `git pull` in the vault clone, by cron or by a push webhook. A submodule would pin a vault commit inside the service repo, making every note you write a service-repo change — backwards.

Consequence worth knowing: **`git pull` is the reindex trigger.** The fsnotify watcher (M2) sees the changed files and patches the index. No reload endpoint, no cache bust, no sync code. A pull lands many files at once, so the watcher's debounce must survive a burst of fifty changed notes without thrashing — test that.

**Downstream consumer:** a personal assistant ("Basim") that reads the vault through this API. He does not exist yet. Do not build for him speculatively — but assume every response is going into an LLM context window, and size it accordingly.

## Non-negotiables

These are architectural commitments, not preferences. If a change would violate one, stop and say so rather than working around it.

1. **Never invent a storage format.** Markdown files on disk are canonical. No database of record, no proprietary index, no note that exists only in the server. If this server is deleted, nothing is lost.
2. **`nullapi` never writes to the vault.** `NULL_VAULT_PATH` is opened `O_RDONLY` unconditionally on that path — enforced at the filesystem layer, not merely by absence of routes. **`nullmcp` writes directly, deliberately** — this was reversed from v0's original "read-only, full stop" design; see "Writes" below for the actual model. Every write is exactly one git commit, one note at a time, never batched: that discipline, not a staging area, is what keeps a bad write cheap to undo.
3. **Never return a body that wasn't explicitly requested.** `/notes` and `/search` return metadata and snippets only. Bodies come from `GET /notes/{path}` alone. This is the rule that keeps a 2,000-note vault from blowing an LLM context window.
4. **Path is the identity.** No UUIDs, no surrogate keys. `engineering/basim/soul.md` addresses that note everywhere, forever.
5. **Never touch dotfiles or dot-directories.** `.git/` above all. Excluded from indexing and unreachable via any route.
6. **The renderer shares the index.** It reads the same in-memory structures as the API — it does not call the API over HTTP, and it does not maintain a second parse. One process, two presentations.

## Writes

`nullmcp` (only — `nullapi`/the renderer stay strictly read-only) writes directly to `NULL_VAULT_PATH` via `create_note`, `write_note`, and `delete_note`. There is no staging area: an earlier design routed writes through a physically separate `NULL_INBOX_PATH` directory precisely to avoid this, and that design was deliberately dropped — the safety model is git discipline instead, enforced by the code, not by physical separation:

- **Every write touches exactly one file, and never shares a commit with a different note.** `create_note` → "Add `<path>`"; `write_note` → "Update `<path>`"; `delete_note` → "Delete `<path>`". `internal/vault/write.go` serializes every stage-then-commit sequence behind a mutex so two concurrent tool calls can never land in the same commit. One refinement: `write_note` amends its own immediately-preceding commit when that commit was itself an unbroken `write_note` update to the same note — editing one note ten times in a row makes one commit, not ten. The moment anything else is committed in between, the chain breaks and the next edit starts fresh. `create_note`/`delete_note` never amend and are never amend targets, no exceptions.
- **This is the undo mechanism.** A bad write is one `git revert` of one specific commit away from gone — never entangled with anything else, because there is never more than one change per commit. There is no confirmation step beyond the tool call itself; git *is* the confirmation step, after the fact.
- **`NULL_VAULT_PATH` must already be a git repository.** Checked once at boot (`vault.EnsureGitRepo`) — a vault that isn't a git repo fails the server's startup, never a write attempt at runtime.
- **Nothing pushes automatically.** `push_vault` is a separate, explicit tool. Local commits sit unpushed until it's called on purpose; it never force-pushes or resolves a conflict itself — a rejected push surfaces git's own error and stops there.

Full contract in `spec/null-mcp-v0.md`.

## Stack

- **Go 1.22+**, stdlib `net/http` with `chi` for routing.
- **No database.** In-memory index, rebuilt on boot, incrementally updated by an `fsnotify` watcher.
- **`ripgrep` shelled out** for full-text search. Not a library, not an inverted index, not embeddings. It is fast to five figures of notes and it can be swapped behind the response shape later.
- `goldmark` for markdown AST (heading extraction) **and HTML rendering**, `goccy/go-yaml` for frontmatter.
- **Renderer: Go `html/template`, server-rendered, no JS framework.** Not a separate app. No React, no Next.js, no build step, no client-side routing. One stylesheet, hand-written.
- Single static bearer token from env. No OAuth, no users, no roles — one human uses this.
- `modelcontextprotocol/go-sdk` for the MCP layer (`cmd/nullmcp`, `internal/mcp`) — the official Go SDK. stdio by default; Streamable HTTP (mutually exclusive, `NULL_MCP_HTTP_ADDR`) for a remote client, bearer-token-or-OAuth-gated, loopback-only, TLS by reverse proxy. `nullmcp` shares nullapi's in-memory index type but writes directly to the vault (see "Writes" above) — `git` shelled out for commits, same "not a library" taste as ripgrep for search.

Chosen because Go is the current backend track and this is a small concurrent I/O service, which is exactly its shape.

## Layout

```
cmd/nullapi/main.go       entrypoint, config, graceful shutdown
cmd/nullmcp/main.go       MCP entrypoint, same vault/search wiring, stdio or HTTP transport
internal/vault/           parse, index, watch — the core
  note.go                 Note struct, frontmatter + heading extraction
  index.go                in-memory index, path→Note, link graph
  watcher.go              fsnotify, incremental reparse
  write.go                CreateNote/WriteNote/DeleteNote/PushVault — the only writes
                           anywhere, straight to the vault, each its own git commit
internal/api/             handlers, middleware, DTOs
  routes.go               chi router
  notes.go, search.go, graph.go
  auth.go, errors.go
internal/search/          ripgrep wrapper, result parsing
internal/render/          html/template handlers, goldmark→HTML, wikilink rewriting
  templates/              layout.html, list.html, note.html, search.html
  static/                 one stylesheet, one font stack. no bundler.
internal/mcp/             MCP tools over the same in-memory index — a second
                           presentation of the read API, and the vault's write path
  tools.go                Tools + typed In/Out structs; no MCP SDK import, testable bare
  server.go                wires Tools to SDK tool handlers + descriptions
  inspector.go             dev-only HTTP page for manual tool calls, opt-in via env
  http.go                  Streamable HTTP transport, bearer-token gated, opt-in via env
  oauth.go                 minimal OAuth 2.1 + DCR for spec-compliant remote clients (Claude.ai)
spec/                     the specs below — read before implementing
```

## Conventions

- Errors as values, wrapped with `fmt.Errorf("%w")`. No panics outside `main`.
- Handlers do HTTP only. All logic in `internal/vault` and `internal/search`, testable without a server.
- Every exported func gets a doc comment stating what it does **and what it assumes**.
- Table-driven tests. Fixture vault under `testdata/vault/` — commit it, keep it small, include a note with unicode, a note with no frontmatter, and a broken wikilink.
- No dependency added without saying why in the commit message.
- Concise commits, imperative mood. No emoji.

## Path safety

Every path from a request goes through one function before touching disk: reject `..`, reject absolute paths, reject symlinks escaping root, `filepath.Clean` then confirm the result is still under vault root. **One function, used everywhere, tested adversarially.** This is the only real attack surface.

## Working style in this repo

- Answer first, then reason. No preamble.
- If a spec is ambiguous, say so and pick the simpler reading — do not implement both.
- Flag scope creep before starting, not after.
- Do not add features from the "deliberately absent" list, however easy they look.
- When you disagree with a decision here, say it once with the reason, then implement what was asked.

## Deliberately absent from v0

Writes through `nullapi` or the renderer, of any kind — the write path is `nullmcp` only, always one commit per note (see "Writes" above) · in-browser editing · embeddings/RAG/chunking · a graph visualization · users, roles, sharing beyond the single static token · pagination beyond a cursor · rate limiting · metrics beyond request logs · any JS build step · automatic pushing — `push_vault` is always a separate, explicit call.

The renderer is a **reader**. The moment it grows a text box it has become an editor, and an editor is the iceberg — years of polish for something your local editor already does better.

Each is a real future need. None is specifiable before the vault has been used through this API for a fortnight.
