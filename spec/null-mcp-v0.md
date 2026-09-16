# Null — MCP v0

Model Context Protocol layer over the vault, for a language model to call
directly instead of going through the JSON API. Same read-only lens, same
non-negotiables, second presentation — not a second implementation.

**Binary:** `cmd/nullmcp`
**Transport:** stdio (newline-delimited JSON-RPC), launched as a subprocess
by the client. HTTP/SSE is supported by the underlying SDK and left as a
documented, unimplemented extension point — see "Deliberately absent"
below.
**Auth:** none over stdio. The trust boundary is the OS process: whoever
can spawn this binary already has the access a bearer token would gate
over HTTP.

---

## Why a separate layer instead of pointing the model at the HTTP API

A tool-calling model reading `/v1/notes` over HTTP still has to know the
URL shape, build query strings, and parse a generic JSON envelope with no
guidance on *when* to call what. MCP tools carry their contract in the
schema and description the client shows the model directly — so the
"never fetch a body you don't need" discipline that's a runtime rule in
the HTTP API becomes something the model is told, in-band, before it ever
calls a tool.

## Tools

All four mirror an HTTP route field-for-field, with one addition:
`list_notes` and `search_notes` results carry `approx_tokens`
(`size_bytes / 4`, a rough heuristic) so the model can budget a `get_note`
call before making it.

### `list_notes`

Mirrors `GET /v1/notes`. Metadata only — this is enforced by the type
system, not just a runtime check: `NoteSummary` has no `Body` field to
leak.

| field | type | notes |
|---|---|---|
| `folder` | string | prefix filter |
| `tags` | string[] | AND semantics |
| `updated_after` | string | ISO 8601 |
| `limit` | int | 1–200, default 50 |
| `cursor` | string | opaque, from a previous `next_cursor` |
| `sort` | string | `path` \| `updated` (default) |

### `get_note`

Mirrors `GET /v1/notes/{path}`. The only tool that returns a body, capped
at the same `NULL_MAX_BODY_BYTES` the HTTP API uses (default 200000); past
that it returns a tool error pointing at `section`.

| field | type | notes |
|---|---|---|
| `path` | string | required, vault-relative |
| `section` | string | exact heading text; slices to that heading |
| `include` | string[] | any of `body` (default), `outlinks`, `backlinks` |

### `search_notes`

Mirrors `GET /v1/search`. Snippets only, same case- and
diacritic-insensitive matching as the HTTP route (`sirr` matches `Şirr`).

| field | type | notes |
|---|---|---|
| `query` | string | required |
| `in` | string | `body` (default) \| `title` \| `both` |
| `folder`, `tags` | | same as `list_notes` |
| `limit` | int | 1–50, default 20 |

### `get_graph`

Mirrors `GET /v1/graph`. BFS neighborhood; every edge carries the line its
wikilink was written on.

| field | type | notes |
|---|---|---|
| `path` | string | required |
| `depth` | int | 1–3, default 1 |
| `direction` | string | `out` \| `in` \| `both` (default) |

## Error semantics

Business errors — unknown path, unresolved cursor, unknown section, body
over cap, bad argument — come back as **tool-level** errors
(`CallToolResult.IsError = true`, message in `Content`), not MCP protocol
errors. This is deliberate and SDK-supported: a protocol error is
invisible to the calling model, so it can't read the message or retry
differently. A tool error is content the model actually sees.

Malformed arguments (wrong JSON type against the schema) are rejected by
the SDK before the handler runs, same effect either way.

## Inspector

`internal/mcp/inspector.go` + `templates/inspector.html`: a server-rendered,
zero-JS HTTP page for manually calling tools during development. It
connects a real MCP client to the running server over an in-memory
transport (`mcp.NewInMemoryTransports`) — the same mechanism the SDK's own
tests use — so what you see is a real protocol round trip, not a shortcut
around one.

Opt-in only: set `NULL_MCP_INSPECTOR_ADDR` (e.g. `127.0.0.1:8090`). Empty
disables it. No auth, keep it on loopback, never run it on the VPS.

## Deliberately absent from v0

- **HTTP/SSE transport.** `internal/mcp.NewServer` is transport-agnostic —
  it returns a plain `*mcp.Server` — so wiring `mcp.NewStreamableHTTPHandler`
  is a small addition later. Not built now because there is no remote
  consumer yet (Basim doesn't exist), and an unauthenticated network
  listener is exactly the kind of thing that must not be guessed at. When
  it is built, it must gate on `NULL_TOKEN` with the same constant-time
  compare `internal/api/auth.go` uses, before it is reachable off loopback.
- **Any tool beyond the four HTTP routes already expose.** No write tools,
  no embeddings/RAG tool, no note-creation tool — same "deliberately
  absent" list as the read API, extended to this surface rather than
  reopened for it.
- **A resources/prompts MCP surface.** Only tools are registered. Notes are
  not exposed as MCP resources; nothing here has needed it yet.
