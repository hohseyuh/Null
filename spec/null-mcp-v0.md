# Null — MCP v0

Model Context Protocol layer over the vault, for a language model to call
directly instead of going through the JSON API. Same in-memory index as
`nullapi`, second presentation of the read side — and, unlike `nullapi`,
the vault's write path too. See CLAUDE.md's "Writes" section for the
architectural commitment this implements, and `spec/tiers.md` for the
curation-tier permission matrix every write here is subject to.

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

Thirteen total: four mirror an HTTP route field-for-field (with one
addition — `list_notes` and `search_notes` results carry
`approx_tokens`, `size_bytes / 4`, a rough heuristic, so the model can
budget a `get_note` call before making it, and both also carry `tier`
on every entry); three are new read capabilities with no HTTP equivalent
(`find_relatives`, `get_links`, `find_path`); three are writes
(`create_note`, `write_note`, `delete_note`) — always registered, no
configuration gate, since writing is now unconditional (see "Writes"
below); three are the model's entire surface onto a note's curation tier
(`tier_get`, `tier_set`, `tier_propose` — see "Tiers" below and
`spec/tiers.md`). There is no fourteenth tool that pushes or commits —
see "One door" in `spec/tiers.md`.

### `list_notes`

Mirrors `GET /v1/notes`. Metadata only — this is enforced by the type
system, not just a runtime check: `NoteSummary` has no `Body` field to
leak.

| field | type | notes |
|---|---|---|
| `folder` | string | prefix filter |
| `tags` | string[] | AND semantics |
| `tier` | string | exact match: `dakhil`/`amil`/`thabit`/`asil` |
| `updated_after` | string | ISO 8601 |
| `updated_before` | string | ISO 8601 — e.g. a staleness sweep over old `dakhil` notes |
| `limit` | int | 1–200, default 50 |
| `cursor` | string | opaque, from a previous `next_cursor` |
| `sort` | string | `path` \| `updated` (default) |

Every result also carries `tier` — non-negotiable, per `spec/tiers.md`.

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
Score is weighted by tier (`spec/tiers.md`'s "Retrieval: weight, do not
filter") — `dakhil` 0.4, `amil` 0.8, `thabit`/`asil` 1.0 — so raw capture
still surfaces, just ranked below what's actually been reviewed.

| field | type | notes |
|---|---|---|
| `query` | string | required |
| `in` | string | `body` (default) \| `title` \| `both` |
| `folder`, `tags`, `tier` | | same as `list_notes` |
| `limit` | int | 1–50, default 20 |

Every result also carries `tier` — non-negotiable, per `spec/tiers.md`.

### `get_graph`

Mirrors `GET /v1/graph`. BFS neighborhood; every edge carries the line its
wikilink was written on. Every node carries its own tier; every edge
carries the **lower** of its two endpoints' tiers — an edge is only as
trustworthy as its weaker end (`spec/tiers.md`). Nothing is excluded by
tier: `/graph` traverses every note regardless.

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
`write_note` for that deliberately. Always lands at tier `dakhil` — any
`tier`, `proposed_*`, `denied_*`, or `tier_history` key in the given
frontmatter is silently stripped, never honored (R1: the model can never
raise a tier, not even at creation).

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
`create_note`. Fails with a tool error if the note is `asil` — immutable
to the model, no exception. If the note is `thabit`, this write demotes
it to `amil` in the same commit (R2); the response's `demoted` field
reports whether that fired, so the model can mention it once, plainly.
`tier`, `proposed_*`, `denied_*`, and `tier_history` are carried forward
from the note's current on-disk state regardless of what frontmatter the
model supplies — a wholesale rewrite can never silently erase them.

### `delete_note`

Removes a note (`git rm` — deletes and stages in one step) and commits
the removal as `"Delete <path>"`. No confirmation step beyond the tool
call itself: the commit *is* the confirmation, after the fact. Undoing a
deletion is a single `git revert` of exactly that commit, never entangled
with any other change, because there is never more than one change per
commit. Only permitted on a `dakhil` note — the permission matrix's one
exception, since `dakhil` is the model's own working space; `amil`,
`thabit`, and `asil` notes return a tool error instead.

| field | type | notes |
|---|---|---|
| `path` | string | required |
| `reason` | string | optional; appended to the commit message body |

### `tier_get`

Read-only, always available. Current tier, when it last changed
(`since` — the note's own `UpdatedAt`, since every tier change coincides
with a write to the file), and any outstanding proposal.

| field | type | notes |
|---|---|---|
| `path` | string | required |

Returns `{path, tier, since, proposed?}`; `proposed`, when present, is
`{tier, reason, at}` read straight off `proposed_tier`/`proposed_reason`/
`proposed_at`.

### `tier_set`

**Lowering only.** Any call that would raise or hold a note's tier
returns a tool error naming R1 — never a silent no-op, because a silent
failure would teach the model the call worked. `reason` is required and
is appended to the note's own `tier_history` in frontmatter (not just
the commit message, so the audit trail survives even a shallow clone),
in a commit of its own: `"Set tier <path>"`. Fails with a tool error if
the note is `asil` — immutable, unconditionally, even to a call that
would only lower it further.

| field | type | notes |
|---|---|---|
| `path` | string | required |
| `tier` | string | required; must be strictly lower than the current tier |
| `reason` | string | required |

### `tier_propose`

Writes a proposal into the note's frontmatter — `proposed_tier`,
`proposed_reason`, `proposed_at` — and changes nothing else: not the
note's tier, not its body. Commits as `"Propose tier <path>"`. The user
acts on it in **Al-Mina** (`GET /mina`, plus the renderer's approve/
deny/defer screen at `/al-mina`). Fails with a tool error if the
note already carries a `denied_tier` equal to the proposed tier and
hasn't been written to since `denied_at` — without this, a denied
proposal would resurface within days and Al-Mina would become a screen
the user stops opening. Also fails if the note is `asil` — there is no
tier above it to propose.

| field | type | notes |
|---|---|---|
| `path` | string | required |
| `tier` | string | required; the tier being proposed |
| `reason` | string | required |

## Writes: the git-commit-per-note model

Every write touches exactly one file and never shares a commit with a
different note — this is the whole safety model, replacing an earlier
design (a physically separate inbox staging directory) that was
deliberately dropped in favor of git discipline enforced by the code:

- **`internal/vault/write.go`** is the only file in this codebase that
  opens a vault file for anything but `O_RDONLY` — `CreateNote`,
  `WriteNote`, `DeleteNote`, `SetTier`, `ProposeTier`. A package-level
  mutex (`gitMu`) serializes every stage-then-commit sequence, so two
  concurrent tool calls can never interleave into a shared commit;
  proven by `TestConcurrentWritesEachGetTheirOwnCommit`. None of them
  ever calls `git push` — see "Tiers" below, "One door".
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
  itself (`NULL_GIT_NAME`/`NULL_GIT_EMAIL`, or the older `NULL_MCP_GIT_*`,
  override the defaults `"nullmcp"`/`"nullmcp@localhost"`; `nullapi` uses
  `"nullapi"` for its own tier commits) — this works regardless of whether
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
  host vault directory as `nullapi`'s service, read-write in both (nullapi
  commits the two human tier actions; see `docs/architecture.md`). The
  vault must be a git repository the container's user can write. The
  Docker image needs `git` installed alongside `ripgrep`.
- **Two processes write one repo** (`nullmcp` here, `nullapi`'s Al-Mina):
  `gitMu` serializes within a process; across processes git's own
  `index.lock` prevents corruption and `runGit` retries briefly on it.

## Tiers

Full contract in `spec/tiers.md`; this section is the MCP-surface summary.
Every note carries a curation tier — `dakhil` (default) / `amil` /
`thabit` / `asil` — enforced entirely server-side, in
`internal/vault/write.go` and `internal/vault/index.go`, never trusted to
the model's instructions:

- **R1 — the model can never raise a tier.** `create_note` always lands
  at `dakhil`; `tier_set` can only lower; `tier_propose` only asks.
  Promotion (`tier_set` going up, or `dakhil`→`amil` on a human's first
  read in the renderer) is unreachable from every tool in this file.
- **R2 — editing a `thabit` note demotes it to `amil`, atomically with
  the edit.** `write_note` does this itself, in the same commit.
- **`asil` is write-locked at the filesystem** (`0444`, maintained by the
  index on every build/reparse), not just by the application-layer check
  — so a bug in the latter still hits a real permission error.
- **`tier`, `proposed_*`, `denied_*`, `tier_history` are server-owned.**
  A model write that sets any of them has them stripped and replaced
  with the server's own values, silently — the model has no legitimate
  reason to set them and no feedback loop to learn from.
- **No MCP tool here exposes `git push` or `git commit`, or a raw
  filesystem write.** The server commits on write; nothing in this file
  ever reaches a remote — see `spec/tiers.md`'s "One door". Earlier
  drafts of this codebase had a `push_vault` tool; it was removed for
  exactly this reason once tiers made "the model has no path to the
  remote at all" the stricter, preferred guarantee.

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

- **`nullapi`/the renderer writing note bodies.** The write path for note
  content is `nullmcp` only — see CLAUDE.md's non-negotiable #2 and
  "Writes" above. (Al-Mina's own approve/deny/defer actions are a
  narrower, structured exception, scoped to the tier field alone — see
  `spec/tiers.md`, implemented in `internal/vault/human.go` and referenced
  only from `internal/render`.)
- **A push or commit tool, of any shape.** Removed deliberately — see
  "Tiers" above, "One door". Nothing in this codebase ever reaches a
  remote by itself; pushing is a human's own `git push`.
- **Any way for the model to raise a tier.** `tier_set` only lowers;
  `tier_propose` only records an ask. Approving a proposal is a human
  action in Al-Mina, unreachable from every tool in this file.
- **Any kind of write confirmation/review step inside the protocol.** Git
  *is* the review mechanism, after the fact (revert a bad commit) rather
  than before it lands. This was a deliberate choice, not an oversight —
  see CLAUDE.md's "Writes" section for the reasoning.
- **Embeddings/RAG.** Same reasoning as the read API: not specifiable
  yet.
- **A resources/prompts MCP surface.** Only tools are registered. Notes
  are not exposed as MCP resources; nothing here has needed it yet.
