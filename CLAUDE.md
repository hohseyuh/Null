# CLAUDE.md — Null

Read-only HTTP API over a markdown vault. This file is the standing context for every session in this repo.

---

## What this is

`Null` is a plain-markdown knowledge vault living on a VPS, under git. This repo is **not** the vault — it is a service that indexes the vault and serves it two ways: a read-only JSON API, and a read-only HTML renderer.

**No Obsidian.** No editor of any kind is part of this system. Notes are authored locally in whatever editor the user prefers and reach the VPS by `git push`; the server pulls or watches the working tree. That git flow is the write path for v0 — the absence of a write API does not mean the vault is static.

Files are the source of truth. This server is a lens over them and holds no state that would survive its own deletion.

## Two repos

**`null-vault`** — the notes. Nothing but `.md` and directories. Private, backed up on its own cadence, no CI, no code.

**`null-service`** — this repo. The Go server. Contains no notes, ever, except the fixture vault under `testdata/`.

**They connect by bind mount, not by submodule.** The vault is cloned to a path on the VPS; the service mounts it read-only and is pointed at it by `NULL_VAULT_PATH`. Sync is a `git pull` in the vault clone, by cron or by a push webhook. A submodule would pin a vault commit inside the service repo, making every note you write a service-repo change — backwards.

Consequence worth knowing: **`git pull` is the reindex trigger.** The fsnotify watcher (M2) sees the changed files and patches the index. No reload endpoint, no cache bust, no sync code. A pull lands many files at once, so the watcher's debounce must survive a burst of fifty changed notes without thrashing — test that.

**Downstream consumer:** a personal assistant ("Basim") that reads the vault through this API. He does not exist yet. Do not build for him speculatively — but assume every response is going into an LLM context window, and size it accordingly.

## Non-negotiables

These are architectural commitments, not preferences. If a change would violate one, stop and say so rather than working around it.

1. **Never invent a storage format.** Markdown files on disk are canonical. No database of record, no proprietary index, no note that exists only in the server. If this server is deleted, nothing is lost.
2. **Never write to the vault.** v0 is read-only, enforced at the filesystem layer (open files `O_RDONLY`), not merely by absence of routes.
3. **Never return a body that wasn't explicitly requested.** `/notes` and `/search` return metadata and snippets only. Bodies come from `GET /notes/{path}` alone. This is the rule that keeps a 2,000-note vault from blowing an LLM context window.
4. **Path is the identity.** No UUIDs, no surrogate keys. `engineering/basim/soul.md` addresses that note everywhere, forever.
5. **Never touch dotfiles or dot-directories.** `.git/` above all. Excluded from indexing and unreachable via any route.
6. **The renderer shares the index.** It reads the same in-memory structures as the API — it does not call the API over HTTP, and it does not maintain a second parse. One process, two presentations.

## Stack

- **Go 1.22+**, stdlib `net/http` with `chi` for routing.
- **No database.** In-memory index, rebuilt on boot, incrementally updated by an `fsnotify` watcher.
- **`ripgrep` shelled out** for full-text search. Not a library, not an inverted index, not embeddings. It is fast to five figures of notes and it can be swapped behind the response shape later.
- `goldmark` for markdown AST (heading extraction) **and HTML rendering**, `goccy/go-yaml` for frontmatter.
- **Renderer: Go `html/template`, server-rendered, no JS framework.** Not a separate app. No React, no Next.js, no build step, no client-side routing. One stylesheet, hand-written.
- Single static bearer token from env. No OAuth, no users, no roles — one human uses this.
- `modelcontextprotocol/go-sdk` for the MCP layer (`cmd/nullmcp`, `internal/mcp`) — the official Go SDK, stdio transport only for now. Same in-memory index as the API and renderer; no second parse.

Chosen because Go is the current backend track and this is a small concurrent I/O service, which is exactly its shape.

## Layout

```
cmd/nullapi/main.go       entrypoint, config, graceful shutdown
cmd/nullmcp/main.go       MCP entrypoint, same vault/search wiring, stdio transport
internal/vault/           parse, index, watch — the core
  note.go                 Note struct, frontmatter + heading extraction
  index.go                in-memory index, path→Note, link graph
  watcher.go              fsnotify, incremental reparse
internal/api/             handlers, middleware, DTOs
  routes.go               chi router
  notes.go, search.go, graph.go
  auth.go, errors.go
internal/search/          ripgrep wrapper, result parsing
internal/render/          html/template handlers, goldmark→HTML, wikilink rewriting
  templates/              layout.html, list.html, note.html, search.html
  static/                 one stylesheet, one font stack. no bundler.
internal/mcp/             MCP tools over the same in-memory index — a second
                           presentation of the read API, not a second implementation
  tools.go                Tools + typed In/Out structs; no MCP SDK import, testable bare
  server.go                wires Tools to SDK tool handlers + descriptions
  inspector.go             dev-only HTTP page for manual tool calls, opt-in via env
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

Writes of any kind · in-browser editing · embeddings/RAG/chunking · a graph visualization · users, roles, sharing · pagination beyond a cursor · rate limiting · metrics beyond request logs · any JS build step.

The renderer is a **reader**. The moment it grows a text box it has become an editor, and an editor is the iceberg — years of polish for something your local editor already does better.

Each is a real future need. None is specifiable before the vault has been used through this API for a fortnight.
