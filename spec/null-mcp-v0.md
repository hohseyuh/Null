# Null — MCP v0

Model Context Protocol layer over the vault, for a language model to call
directly instead of going through the JSON API. Same read-only lens, same
non-negotiables, second presentation — not a second implementation.

**Binary:** `cmd/nullmcp`
**Transport:** two modes, mutually exclusive, chosen by whether
`NULL_MCP_HTTP_ADDR` is set — never both in one process (see "HTTP
transport" below for why).
- **stdio** (default): newline-delimited JSON-RPC, launched as a
  subprocess by the client (Claude Desktop, Claude Code, an SSH command).
  **Auth:** none. The trust boundary is the OS process — whoever can
  spawn this binary already has the access a bearer token would gate.
- **Streamable HTTP** (`NULL_MCP_HTTP_ADDR` set): for a remote client
  that can't spawn a local subprocess — a browser-based one like
  Claude.ai's connector settings, most concretely. **Auth:** `NULL_TOKEN`
  required, checked on every request with the same constant-time compare
  `internal/api/auth.go` uses. Mandatory, not optional, the moment this
  mode is on — see below.

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

Eleven total: four mirror an HTTP route field-for-field (with one
addition — `list_notes` and `search_notes` results carry
`approx_tokens`, `size_bytes / 4`, a rough heuristic, so the model can
budget a `get_note` call before making it); three are new read
capabilities with no HTTP equivalent (`find_relatives`, `get_links`,
`find_path`); two more are "vault only" companions to `get_graph` and
`find_path` (`get_graph_vault_only`, `find_path_vault_only`), always
registered regardless of inbox configuration; two are writes, registered
only when `NULL_INBOX_PATH` is configured (`create_note`, `write_note`).

Every tool result that carries a title runs it through the inbox label
(see "Inbox" below) — this is not opt-in per call. A model reading any
`list_notes`/`search_notes`/`get_note`/`get_graph`/`find_relatives`/
`get_links`/`find_path` result sees, unmissably, which notes are settled
vault content and which are its own (or another session's) unreviewed
drafts.

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

Includes inbox content: the root may itself be an inbox note, and inbox
nodes appear wherever the traversal reaches them (which today, given the
cross-boundary limitation below, only happens when the root itself is on
the inbox side).

### `get_graph_vault_only`

Same shape and params as `get_graph`, restricted to the vault
unconditionally — registered whether or not `NULL_INBOX_PATH` is set, and
guarantees zero inbox exposure: an inbox-prefixed `path` is rejected as
"no such note" before any traversal happens, never silently walked. Use
this over `get_graph` when the caller specifically needs to know the
answer holds regardless of whatever is currently sitting in the inbox —
e.g. checking whether something is already established before drafting a
new note about it.

### `find_relatives`

No HTTP equivalent. Notes related by **folder and/or shared tags** —
organizational/topical proximity, independent of the link graph entirely.
Two notes can be relatives with zero links between them; two linked notes
need not be relatives.

| field | type | notes |
|---|---|---|
| `path` | string | required |
| `by` | string | `folder` \| `tags` \| `both` (default) |
| `limit` | int | 1–100, default 20 |

Each result carries `same_folder` (bool) and `shared_tags` (string[]),
sorted by shared-tag count then folder match.

### `get_links`

No HTTP equivalent. One note's direct outlinks and backlinks as two
separate lists, each entry carrying the link's context line — a cheaper
read than `get_graph(depth=1, direction=both)` when direction is what
matters and a second hop isn't needed.

| field | type | notes |
|---|---|---|
| `path` | string | required |

### `find_path`

No HTTP equivalent. The shortest chain of wikilinks connecting two
*specific* notes — "how, if at all, are these related" — as opposed to
`get_graph`'s "what surrounds this one note." `found: false` with an empty
`path` means no route within the depth searched; that's a normal result,
not an error, the same way a zero-hit search isn't one.

| field | type | notes |
|---|---|---|
| `from`, `to` | string | required |
| `depth` | int | max hops, 1–6, default 4 |
| `direction` | string | `out` \| `in` \| `both` (default) |

Either endpoint may be an inbox note.

### `find_path_vault_only`

Same shape and params as `find_path`, restricted to the vault
unconditionally — registered whether or not `NULL_INBOX_PATH` is set.
Either `from` or `to` being an inbox-prefixed path fails as "no such
note," the same guarantee `get_graph_vault_only` makes.

### `create_note` / `write_note` — inbox only

Registered only when `NULL_INBOX_PATH` is set. `path` must start with
`inbox/`; anything else is rejected before touching disk. `create_note`
fails if a note already exists there (`ErrNoteExists`); `write_note` fails
if one doesn't (`ErrNoteNotFound`) — deliberately no upsert, so a typo'd
path can't silently create a stray note or silently clobber an existing
one.

| field | type | notes |
|---|---|---|
| `path` | string | required; `inbox/...` |
| `frontmatter` | object | optional |
| `body` | string | required |

Both call `vault.Index.Refresh` on the inbox index synchronously after a
successful write, so the very next `get_note`/`list_notes`/`search_notes`
call sees the change immediately — no race against the watcher's 200ms
debounce.

## Inbox

A second directory, `NULL_INBOX_PATH`, physically separate from
`NULL_VAULT_PATH` and never part of the `null-vault` git repo — see
CLAUDE.md's "Inbox" section for the full rationale. In this package:

- **`internal/vault/write.go`** — `CreateNote`/`WriteNote`, the only code
  in this repository that opens a file for anything but `O_RDONLY`, and
  only ever against `NULL_INBOX_PATH`.
- **`internal/vault/combined.go`** — `Combined` merges a vault `Index` and
  an inbox `Index` into one `Reader`, prefixing every inbox path with
  `inbox/` (`mcp.InboxPrefix`) so it can never collide with a real vault
  path. `Tools.Index` is typed `vault.Reader` precisely so it can hold
  either a plain `Index` (no inbox configured) or a `Combined`.
- **Search** is a separate `ripgrep` process per root — `Tools.Search`
  over the vault, `Tools.InboxSearch` over the inbox (nil when
  unconfigured) — merged by `SearchNotes` the same way `Combined` merges
  reads.
- **Known v0 limitation:** link resolution, backlinks, and `get_graph`/
  `find_path` traversal do not cross the vault/inbox boundary. A draft's
  wikilink to a real note (or a real note's link to something that will
  later live in the inbox — not a real scenario since inbox notes don't
  exist yet at promotion time, but stated for completeness) stays
  unresolved until the note is promoted, at which point it's just a
  normal note in a normal `Index` and resolves normally. `Resolve` (used
  by would-be renderer integration, not currently wired to `nullapi`) is
  the one method that does cross the boundary, because a plain
  target→path lookup is cheap and safe in a way pre-computing every
  cross-boundary graph edge is not.
- **Boot-time guard:** a vault with a literal top-level `inbox/` directory
  fails `nullmcp` startup rather than silently shadowing real notes under
  the reserved namespace.

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

## HTTP transport

`internal/mcp/http.go`. Set `NULL_MCP_HTTP_ADDR` to switch `nullmcp` from
stdio to the MCP Streamable HTTP transport (`mcp.NewStreamableHTTPHandler`),
mounted at `/mcp`, plus an unauthenticated `GET /healthz` for liveness —
no vault content or tool schemas in that response, just a 200.

**Why mutually exclusive with stdio, never both in one process:** stdio
mode's whole trust model is "whoever can spawn this process already has
access" — that assumption breaks the moment the process is a long-running
daemon nothing is piping stdin into. A backgrounded process whose stdin
hits EOF immediately (no client attached) triggers a clean shutdown by
design — correct behavior for stdio, fatal for a daemon. This happened
during development the first time the binary was smoke-tested
backgrounded without a client on stdin: it exited within milliseconds.
The fix is architectural, not defensive code — HTTP mode never touches
`&sdkmcp.StdioTransport{}` at all.

**Auth is mandatory the moment this mode is on**, not optional. `/mcp`
accepts either the raw `NULL_TOKEN` as a bearer header (constant-time
compare, same discipline `internal/api/auth.go` uses — fine for curl,
testing, or any client that doesn't need the dance below) or a valid
OAuth access token (see "OAuth" below). `loadConfig` refuses to boot
with `NULL_MCP_HTTP_ADDR` set and no `NULL_TOKEN` or `NULL_MCP_PUBLIC_URL`
— enforced at startup, not left as a runtime possibility.

### OAuth

`internal/mcp/oauth.go`. The MCP authorization spec — which Claude.ai's
connector UI requires, specifically — expects a real OAuth 2.1 flow with
Dynamic Client Registration, not a header a human pastes in. This is
that flow, implemented to exactly the extent the spec requires and no
further: RFC 9728 (protected resource metadata), RFC 8414 (authorization
server metadata), RFC 7591 (dynamic client registration), RFC 8707
(resource indicators / audience binding), PKCE (S256 only — "plain" is
not accepted), single-use authorization codes, rotated refresh tokens.

There is exactly one real credential anywhere in this flow: `NULL_TOKEN`,
the same one `/mcp` already accepted directly. OAuth here is a protocol
envelope around that one secret, not a user system — "no users, no
roles, one human uses this" is still true. The `/authorize` step is a
single password-style form (`internal/mcp/oauth.go`'s embedded HTML)
that checks the submitted value against `NULL_TOKEN`, constant-time,
same as everywhere else. There is no separate OAuth client
ID/secret to configure by hand — a compliant client (Claude.ai included)
self-registers via DCR and discovers everything else from the metadata
endpoints.

**Endpoints**, all under the same `NULL_MCP_PUBLIC_URL` base:

| path | method | purpose |
|---|---|---|
| `/.well-known/oauth-protected-resource` | GET | names the authorization server (RFC 9728) |
| `/.well-known/oauth-authorization-server` | GET | authorization server metadata (RFC 8414) |
| `/register` | POST | dynamic client registration (RFC 7591) |
| `/authorize` | GET, POST | the credential check; GET shows the form, POST checks it |
| `/token` | POST | `authorization_code` and `refresh_token` grants |

**Security properties actually verified, not just claimed** (see
`internal/mcp/oauth_test.go`'s `TestOAuthFullFlow` and friends, plus a
live curl-driven run of the whole flow against a real running process
during development):
- PKCE is mandatory — `/authorize` without `code_challenge`/S256 is
  rejected before a credential is even asked for.
- `redirect_uri` must exactly match one registered via DCR — a mismatch
  fails *in place*, never redirects anywhere. Redirecting on that
  specific failure is the open-redirect vulnerability the spec calls out
  by name; this deliberately does not do that.
- Authorization codes are single-use (deleted on first exchange attempt,
  success or failure) and short-lived (5 min).
- The `resource` parameter is validated at both `/authorize` and
  `/token` against this server's own canonical resource URI
  (`NULL_MCP_PUBLIC_URL` + `/mcp`) — a token can't be requested for, or
  used against, a different resource.
- Refresh tokens rotate: each use invalidates the old one and issues a
  new one, so a stolen-then-reused refresh token is a detectable replay,
  not a silent one.

**`NULL_MCP_PUBLIC_URL`** must be this server's own public HTTPS origin
(e.g. `https://host:10000`, no trailing slash) — every URL in the
metadata documents, and the resource identifier tokens are bound to, is
derived from it. Wrong value, and discovery or audience validation
breaks for every client, not just misbehaves quietly.

**`DisableLocalhostProtection: true`** is set on the SDK's DNS-rebinding
guard. This handler is designed to sit behind a local reverse proxy
(Tailscale Funnel, in this repo's own deployment) that connects via
loopback while forwarding the original public `Host` header — exactly
what that guard exists to catch under normal circumstances. The bearer
token is the actual security boundary for this transport; the
localhost/Host-header check is a different, narrower protection this
specific topology doesn't need.

**Deployment shape** (`compose.yaml`'s `nullmcp` service, `deploy/`):
loopback-only (`127.0.0.1:${NULL_MCP_HOST_PORT:-8092}`), TLS terminated
by whatever reverse-proxies it — Tailscale Funnel in this repo's own
deployment — never by this process itself. Same non-negotiable as
everywhere else in this stack: this binary does not do TLS.

## Deliberately absent from v0

- **Any tool that writes to the vault.** `create_note`/`write_note` only
  ever open files under `NULL_INBOX_PATH`; there is no tool, and there
  must never be one, that opens anything but `O_RDONLY` under
  `NULL_VAULT_PATH`.
- **A promotion tool.** Moving a reviewed inbox note into the vault is a
  human `git commit`, not a server action — see CLAUDE.md's "Inbox"
  section. Automating that step is automating the review it exists to
  force.
- **Cross-boundary graph edges** (see "Inbox" above) — a real gap, kept
  open deliberately rather than built around, since the correct fix
  (merging resolution, not just reads, across two indices) is bigger than
  this pass and not yet worth it until promoted-vs-draft linking has come
  up in practice.
- **Embeddings/RAG, in either the vault or the inbox.** Same reasoning as
  the read API: not specifiable yet.
- **A resources/prompts MCP surface.** Only tools are registered. Notes are
  not exposed as MCP resources; nothing here has needed it yet.
