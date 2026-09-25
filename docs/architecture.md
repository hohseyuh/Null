# Architecture

How Null works, who can do what, and which test enforces each guarantee.
Read this after [`CLAUDE.md`](../CLAUDE.md) (the rules) and before changing
anything security-relevant. For *why* things are the way they are, see
[`decisions.md`](decisions.md).

## What Null is

A folder of markdown files (the **vault**, kept under git) plus a Go service
that indexes it and presents it three ways to three different kinds of user:

| Presentation | Binary | Audience | Reads | Writes |
|---|---|---|---|---|
| JSON API (`/v1/*`, `/mina`) | `nullapi` | a program | yes | never |
| Web reader (`/`, `/n/*`, `/graph`, `/al-mina`, `/setup`) | `nullapi` (same process) | the human owner | yes | two tier actions + choosing the vault |
| MCP tools | `nullmcp` | a language model | yes | notes + tier lowering/proposing |

The vault is the only source of truth. The service keeps an in-memory index
(rebuilt on boot, patched by a file watcher) and holds no state that would
survive its deletion. The one thing it persists of its own is a tiny config
file recording which folder is the vault.

## The idea that shapes everything: the model is not trusted

A language model can write notes here. So the design assumes anything the
model writes — including note text that ends up rendered as HTML in the
owner's browser — may be wrong or adversarial. Two consequences:

1. **Curation is server-enforced, not requested.** Every note has a *tier*
   (`dakhil` → `amil` → `thabit` → `asil`) meaning "how far a human has
   reviewed this". The model can lower a tier or *propose* a raise; only the
   human can raise one. This is code that returns errors, not a prompt.
2. **Human actions must be unforgeable by the model.** Because the browser UI
   can now write (approve a proposal, promote on read), everything the model
   can place in front of the browser — note bodies — is treated as hostile.

Tier semantics and the permission matrix are in [`spec/tiers.md`](../spec/tiers.md).

## Processes and data flow

```
                       ┌────────────────────────── nullapi (one process) ───────────────────────────┐
 browser ──cookie/UI──▶│ session.Guard ─▶ render  (list, note, search, graph, al-mina) ──┐            │
 token                 │        │                                                        │ human.go   │
                       │        └────────▶ setup  (/setup folder picker) ──▶ app.Manager │ (tier      │
 program ──API token──▶│ api.requireBearer ─▶ api  (/v1/*, /mina, read-only)             │  writes)   │
                       │                       │                                         ▼            │
                       │                       └──── shared vault.Index + watcher ──── git commit     │
                       └───────────────────────────────────────────────────────────────┬─────────────┘
                                                                                        │ same directory
 model ──MCP (stdio, or HTTPS+OAuth)──▶ nullmcp: mcp.Tools ─▶ vault.Index (own copy)    │ (bind mount)
                                                   └─ vault/write.go ─▶ git commit ─────┴──▶ vault/ (git repo)
```

- **Two processes, two indexes, one directory.** `nullapi` and `nullmcp` each
  build their own index and run their own file watcher over the same folder;
  fsnotify keeps them consistent. After its own write, each process refreshes
  the touched file synchronously so it never races its own watcher's 200 ms
  debounce.
- **Two writers, one git repo.** In-process, `gitMu` serializes every
  stage-then-commit. Across processes, git's own `index.lock` prevents
  corruption and `runGit` retries briefly when the other process holds it.
- **`nullapi` is an `app.Manager`.** It owns the current vault's index, watcher,
  search, API router and renderer, and can replace all of them at runtime (that
  is how choosing a vault at `/setup` needs no restart). With no vault chosen it
  runs in *setup mode* and serves only `/login` and `/setup`.

## Who can do what

| Actor | Credential | Can read | Can write |
|---|---|---|---|
| Program using the HTTP API | `NULL_TOKEN` (bearer) | `/v1/*`, `/mina` | nothing |
| Model, via MCP over stdio | none — the OS process boundary | all read tools | `create_note`, `write_note`, `delete_note` (subject to the tier matrix), `tier_set` (lower only), `tier_propose` |
| Model, via MCP over HTTPS | `NULL_TOKEN` bearer or OAuth 2.1 (wrapped around the same secret) | same | same |
| Human in the browser | `NULL_UI_TOKEN` (cookie or bearer) | whole web UI | promote on first open; approve/deny/defer proposals; choose the vault |
| Human in an editor + git | filesystem | — | anything — this path is outside the service and always allowed |

**The API token and the UI token should differ.** If they are equal, anything
holding the API token can log in to the UI and approve its own proposals. The
server warns at boot when they match.

### The tier permission matrix (what the model may do to a note)

| Note's tier | create | edit (`write_note`) | delete | `tier_set` |
|---|---|---|---|---|
| `dakhil` | — (new notes start here) | yes | yes | can't lower further |
| `amil` | — | yes | no | lower only |
| `thabit` | — | yes, and it **demotes to `amil` in the same commit** (R2) | no | lower only |
| `asil` | — | no — also `0444` on disk | no | no |

Rule **R1**: nothing the model can call ever raises a tier. Server-owned
frontmatter — `tier`, `proposed_*`, `denied_*`, `tier_history` — is stripped
from anything the model writes and replaced with the server's own values.

## The write paths (every one is one git commit for one note)

1. **Model → note content.** `mcp.Tools.CreateNote/WriteNote/DeleteNote` →
   `vault.SafeRequestPath` (path gate) → `vault/write.go` (permission checks,
   field stripping, R2) → `git add` + `git commit` under `gitMu` →
   `Index.Refresh`. Consecutive `write_note` calls on the same note amend the
   previous commit instead of stacking ten.
2. **Model → tier metadata.** `SetTier` (lower only) and `ProposeTier` in the
   same file; metadata-only rewrites keep the body byte-stable.
3. **Human → tier metadata.** `vault/human.go`: `MarkOpened` (first open),
   `ApproveProposal`, `DenyProposal`, `DeferProposal`. Called only from
   `internal/render`. Approving to `asil` write-locks the file immediately.
4. **Human → which folder is the vault.** `/setup` → `app.Manager.Start` →
   `config.Save`. Writes a config file, not the vault.

There is **no push and no commit tool for the model**; the server commits on
write and never contacts a remote. Publishing is the human's own `git push`.

## Web UI security model

The renderer serves model-authored HTML and performs writes, so:

- **CSP on every page**: only same-origin scripts, no inline script, no frames,
  forms post only to self.
- **CSRF token** (HMAC of the UI token) on every write form, plus a
  `Sec-Fetch-Site`/`Origin` same-origin check on every POST. A form planted in a
  note is same-origin but cannot know the token.
- **Promotion on first open counts only a real user navigation**: cookie session
  (not the `Authorization` header) *and* `Sec-Fetch-User: ?1` *and*
  `Sec-Fetch-Dest: document`. An `<img>`, prefetch, iframe, script, meta-refresh
  or API call inside a note cannot produce those. A client sending no
  `Sec-Fetch-*` headers never promotes (the safe default).
- **`/setup` is confined** to `NULL_BROWSE_ROOT`: symlinks resolved before the
  containment check, dot-directories refused, only directories listed, relative
  paths and NUL bytes rejected.

## Routes

`nullapi` (all behind `NULL_TOKEN` unless noted; contract in
[`spec/null-read-api-v0.md`](../spec/null-read-api-v0.md)):
`GET /v1/health` (open), `/v1/notes`, `/v1/notes/{path}`, `/v1/search`,
`/v1/graph`, `/mina`.

`nullapi` web UI (behind `NULL_UI_TOKEN`; `/login?token=` sets the cookie):
`/`, `/n/{path}`, `/s`, `/graph` + `/graph/data`, `/al-mina` (GET) +
`/al-mina/act` (POST), `/setup`, `/static/*`. Before a vault is chosen, `/`
redirects to `/setup` and API routes return `503 not_configured`.

`nullmcp` over HTTP: `/mcp` (Streamable HTTP), `/healthz`, and the OAuth
endpoints (`/.well-known/oauth-*`, `/register`, `/authorize`, `/token`).
Contract in [`spec/null-mcp-v0.md`](../spec/null-mcp-v0.md).

## Configuration

All environment variables. `nullapi` only unless noted.

| Variable | Default | Purpose |
|---|---|---|
| `NULL_TOKEN` | required | API bearer token (also `nullmcp`'s HTTP token) |
| `NULL_UI_TOKEN` | = `NULL_TOKEN` | browser UI / Al-Mina / `/setup` token |
| `NULL_VAULT_PATH` | unset | fixes the vault (`/setup` becomes read-only). Also read by `nullmcp` |
| `NULL_CONFIG_PATH` | `<user config dir>/null/config.json` | saved vault choice. Also read by `nullmcp` |
| `NULL_BROWSE_ROOT` | `$HOME` | the only area `/setup` may browse; only needed when the vault is chosen in the browser |
| `NULL_ADDR` | `:8080` | listen address |
| `NULL_MAX_BODY_BYTES` | `200000` | single-note body cap (413 past it) |
| `NULL_MINA_STALE_DAYS` | `14` | age at which an untouched `dakhil` note appears in Al-Mina |
| `NULL_GIT_NAME` / `NULL_GIT_EMAIL` | binary name | author of server-made commits (`NULL_MCP_GIT_*` still honoured) |
| `NULL_MCP_HTTP_ADDR` | unset | `nullmcp`: switch from stdio to Streamable HTTP (mutually exclusive) |
| `NULL_MCP_PUBLIC_URL` | required with the above | `nullmcp`: public https origin, for OAuth discovery |
| `NULL_MCP_STATE_PATH` | unset (in memory) | `nullmcp`: persist OAuth clients and tokens (hashed, `0600`) across restarts. Set it on any host restarted often |
| `NULL_MCP_INSPECTOR_ADDR` | unset | `nullmcp`: dev-only tool-calling page, loopback only |

**Vault resolution order:** `NULL_VAULT_PATH` → saved config file → setup mode.

## Deployment shapes

- **`compose.yaml`** — vault fixed by `NULL_VAULT_PATH=/vault` (host folder
  `VAULT_PATH` mounted there). Starts `nullapi` and `nullmcp`.
- **`compose.setup.yaml`** — vault chosen in the browser; host folder
  `VAULTS_DIR` mounted at `/vaults` (the browse root); choice saved in a named
  volume; `nullmcp` is a profile started after a vault is chosen.
- **Any host + Tailscale Funnel** (a laptop, the VPS, the VAIO) — `nullmcp` on
  loopback, Funnel as the reverse proxy; see "Running `nullmcp` on a laptop" in
  the README. Nothing in the code is host-specific; only `NULL_MCP_PUBLIC_URL`
  changes when moving.
- **Bare metal** — `deploy/nullapi.service` (systemd), or just run the binaries.
- **Both** mount the vault **read-write** (writes are git commits made
  in-container) and need the vault to be a git repository writable by the
  container's user (`NULL_UID`/`NULL_GID`). Neither terminates TLS.

## Where each guarantee is tested

| Guarantee | Test(s) |
|---|---|
| Lists/search never return a body | `TestListNotesMetadataOnly` (api, mcp) |
| One path gate; traversal/symlink escapes rejected | `TestSafeRequestPath`, `…SymlinkEscape`, `TestGetNotePathSafety` |
| Watcher survives a `git pull` burst | `TestWatcherGitPullBurst` |
| Every write = one commit, one file | `TestCreateNoteCommitsExactlyOneFile`, `TestConcurrentWritesEachGetTheirOwnCommit` |
| Repeated edits collapse into one commit | `TestConsecutiveWriteNoteCallsCollapseIntoOneCommit` |
| R1: model can't raise a tier | `TestTierSetEveryRaiseFails`, `TestTierSetNeverRaises`, `TestCreateNoteIgnoresModelSuppliedTier` |
| R2: editing `thabit` demotes atomically | `TestWriteNoteDemotesThabitToAmil` |
| `asil` immutable, incl. at the filesystem | `TestAsilWriteLockedAtFilesystem`, `TestWriteNoteRefusesAsilAtEveryBoundary` |
| Server-owned frontmatter stripped | `TestModelWriteCannotSetServerOwnedFields` |
| No MCP tool exposes git/push/commit | `TestNoToolExposesGitOrFilesystemWrite` |
| Only the renderer can call tier-raising code | `TestOnlyTheRendererCanRaiseTiers` |
| Forged/cross-site approvals rejected | `TestMinaRejectsForgedPosts` |
| Promotion only on genuine user navigation | `TestFirstOpenPromotesOnlyGenuineUserNavigation` |
| CSP on every page | `TestEveryPageCarriesCSP` |
| Setup confined to the browse root | `TestResolveConfinesToRoot`, `TestChoosingAVaultActivatesAndPersists` |
| Env-fixed vault can't be changed from the browser | `TestEnvLockedVaultCannotBeChangedFromTheBrowser` |
| OAuth: PKCE required, redirect mismatch fails closed | `TestOAuthRequiresPKCE`, `TestOAuthRedirectURIMismatchFailsClosed` |
| OAuth state survives a restart; stores no raw tokens; spent refresh tokens stay spent; other-origin state discarded | `TestOAuthStateSurvivesARestart`, `…HoldsNoUsableSecrets`, `…RotatedRefreshTokenStaysSpent…`, `…ForAnotherOriginIsDiscarded`, `TestOAuthBadStateFileNeverStopsBoot` |
| No token or credential ever reaches a log line | `TestNoSecretsReachTheLogs` |
| Compact JSON on the wire | `TestCompactJSONWireFormat`, `TestGraphPageAndCompactData` |

## Status and open items

Built: read API, web reader, MCP layer (13 tools), OAuth for remote MCP
clients, the whole tier system including Al-Mina, graph view and first-run
setup, CI, container images.

Not done:
- **`epistemics.md`** — `spec/tiers.md` calls for an `asil` note in the *vault*
  that teaches any model how to read each tier. It lives in the user's vault, not
  this repo, and has not been written yet.
- **Redeploy** of the existing remote instance (it predates direct writes and
  tiers).
- The Al-Mina "what changed since last review" field is a boolean
  (`changed_since_review`), not a diff.
- The graph view has been exercised under a scripted DOM harness and against the
  running server, not driven in a real browser session.
