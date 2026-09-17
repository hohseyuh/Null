# Null — MCP v0

Model Context Protocol layer over the vault, for a language model to call
directly instead of going through the JSON API. Same in-memory index as
`nullapi`, second presentation of the read side — and, unlike `nullapi`,
the vault's write path too. See CLAUDE.md's "Writes" section for the
architectural commitment this implements.

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
`find_path`); four are writes (`create_note`, `write_note`,
`delete_note`, `push_vault`) — always registered, no configuration gate,
since writing is now unconditional (see "Writes" below).

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

Mirrors `GET /v1/notes/{path}`. The only read tool that returns a body,
capped at the same `NULL_MAX_BODY_BYTES` the HTTP API uses (default
200000); past that it returns a tool error pointing at `section`.

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

### `create_note`

Writes a brand-new note directly into the vault and commits it — `git
add` + `git commit -m "Add <path>[, plus reason]" -- <path>`, exactly
that one file, nothing else staged or swept in. Fails with a clear error
if a note already exists at that path (never a silent overwrite); use
`write_note` for that deliberately.

| field | type | notes |
|---|---|---|
| `path` | string | required, vault-relative |
| `frontmatter` | object | optional |
| `body` | string | required |
| `reason` | string | optional; appended to the commit message body |

Calls `vault.Index.Refresh` synchronously after the commit, so the very
next `get_note`/`list_notes`/`search_notes` call sees the change
immediately — no race against the watcher's 200ms debounce.

### `write_note`

Overwrites an existing note wholesale — the given body and frontmatter
replace what was there entirely, not a merge or a patch — and commits it
as `"Update <path>"`. Fails if nothing exists yet at that path; use
`create_note` for a new one. Same fields and `Refresh` behavior as
`create_note`.

### `delete_note`

Removes a note (`git rm` — deletes and stages in one step) and commits
the removal as `"Delete <path>"`. No confirmation step beyond the tool
call itself: the commit *is* the confirmation, after the fact. Undoing a
deletion is a single `git revert` of exactly that commit, never entangled
with any other change, because there is never more than one change per
commit.

| field | type | notes |
|---|---|---|
| `path` | string | required |
| `reason` | string | optional; appended to the commit message body |

### `push_vault`

Pushes every local commit made by `create_note`/`write_note`/
`delete_note` since the last push to the vault's configured git remote.
No parameters. Deliberately separate from every write tool — nothing
leaves this server until this is called on purpose. A rejected push
(non-fast-forward, no remote configured, etc.) is reported as a tool
error carrying git's own message; this tool never force-pushes and never
attempts to resolve a conflict itself — that decision belongs to a human
looking at the actual conflicting history.

## Writes: the git-commit-per-note model

Every write touches exactly one file and never shares a commit with a
different note — this is the whole safety model, replacing an earlier
design (a physically separate inbox staging directory) that was
deliberately dropped in favor of git discipline enforced by the code:

- **`internal/vault/write.go`** is the only file in this codebase that
  opens a vault file for anything but `O_RDONLY` — `CreateNote`,
  `WriteNote`, `DeleteNote`, `PushVault`. A package-level mutex
  (`gitMu`) serializes every stage-then-commit sequence, so two
  concurrent tool calls can never interleave into a shared commit;
  proven by `TestConcurrentWritesEachGetTheirOwnCommit`.
- **`write_note` amends instead of stacking, when it safely can.** If
  the immediately-preceding commit (`HEAD`) is itself an unbroken
  `write_note` update to the *same* path — checked strictly: an exact
  `"Update <rel>"` header line, the tool's own `Source: nullmcp
  write_note` trailer present, and `HEAD` touching nothing but `rel` —
  the new content amends that commit rather than creating another one.
  Editing one note ten times in a row inside one session produces one
  commit, not ten. The chain breaks the instant anything else is
  committed in between (a different note, a `create_note`, a
  `delete_note`), and the next `write_note` starts fresh — proven by
  `TestConsecutiveWriteNoteCallsCollapseIntoOneCommit` and
  `TestWriteNoteDoesNotAmendAcrossADifferentCommit`. On each amend the
  message is fully regenerated from the *current* call's `reason`, so
  only the latest reason survives; stale ones from earlier edits in the
  chain don't accumulate. `create_note` and `delete_note` never amend
  and are never amend targets — `TestCreateAndDeleteNeverAmend`.
- **`vault.EnsureGitRepo`** is checked once at `nullmcp` boot: a
  `NULL_VAULT_PATH` that isn't a git repository fails startup, never a
  write attempt at runtime.
- **Commit identity** comes from `GIT_AUTHOR_NAME`/`GIT_AUTHOR_EMAIL`/
  `GIT_COMMITTER_NAME`/`GIT_COMMITTER_EMAIL` set on the git subprocess
  itself (`NULL_MCP_GIT_NAME`/`NULL_MCP_GIT_EMAIL` override the defaults,
  `"nullmcp"`/`"nullmcp@localhost"`) — this works regardless of whether
  the runtime environment has any git identity configured on disk, which
  a container typically won't.
- **`safe.directory=*`** is passed on every git invocation. Without it, a
  git process running as a container user against a bind-mounted volume
  owned by a different host UID — the normal shape of this deployment —
  refuses to operate at all ("detected dubious ownership"). The
  protection this disables doesn't apply here: `NULL_VAULT_PATH` is one
  hardcoded, operator-chosen path this process is built to write to, not
  an arbitrary directory it wanders into.
- **Deployment**: `compose.yaml`'s `nullmcp` service mounts the *same*
  host vault directory as `nullapi`'s service, but without `:ro` — the
  one write-capable mount in the whole deployment. The Docker image
  needs `git` installed alongside `ripgrep` for this to work at all.

## Error semantics

Business errors — unknown path, unresolved cursor, unknown section, body
over cap, bad argument, a failed write or push — come back as
**tool-level** errors (`CallToolResult.IsError = true`, message in
`Content`), not MCP protocol errors. This is deliberate and
SDK-supported: a protocol error is invisible to the calling model, so it
can't read the message or retry differently. A tool error is content the
model actually sees.

Malformed arguments (wrong JSON type against the schema) are rejected by
the SDK before the handler runs, same effect either way.

## Inspector

`internal/mcp/inspector.go` + `templates/inspector.html`: a server-rendered,
zero-JS HTTP page for manually calling tools during development. It
connects a real MCP client to the running server over an in-memory
transport (`mcp.NewInMemoryTransports`) — the same mechanism the SDK's own
tests use — so what you see is a real protocol round trip, not a shortcut
around one. This includes the write tools: using it against a real vault
creates real commits, same as any other client.

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

- **`nullapi`/the renderer writing.** The write path is `nullmcp` only —
  see CLAUDE.md's non-negotiable #2 and "Writes" above.
- **A promotion tool, in the old inbox-staging sense.** There's nothing
  left to promote — `create_note`/`write_note`/`delete_note` already
  write straight to the vault. What remains a deliberate human act is
  `push_vault`: nothing reaches the remote without that explicit call.
- **Force-push or automatic conflict resolution in `push_vault`.** A
  rejected push is reported and stops there — resolving diverged history
  is a human decision, not a default this tool guesses at.
- **Any kind of write confirmation/review step inside the protocol.** Git
  *is* the review mechanism, after the fact (revert a bad commit) rather
  than before it lands. This was a deliberate choice, not an oversight —
  see CLAUDE.md's "Writes" section for the reasoning.
- **Embeddings/RAG.** Same reasoning as the read API: not specifiable
  yet.
- **A resources/prompts MCP surface.** Only tools are registered. Notes
  are not exposed as MCP resources; nothing here has needed it yet.
