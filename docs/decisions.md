# Decisions

Why Null is shaped the way it is — especially the places where an earlier
design was **deliberately reversed**, so nobody "restores" something that was
removed on purpose. Newest reasoning wins; each entry says what changed and
what it cost. For how the system works today, see
[`architecture.md`](architecture.md).

## Foundations (unchanged since v0)

**Markdown files on disk are the only storage.** No database of record, no
proprietary index. The service is a lens; delete it and nothing is lost. The
index is in memory, rebuilt on boot, patched by an fsnotify watcher (`git pull`
is the reindex trigger; the debounce must survive a fifty-file burst).

**Path is the identity.** No UUIDs. `engineering/basim/soul.md` names a note
everywhere, forever. One function (`vault.SafeRequestPath`) gates every
request-supplied path.

**Never return a body nobody asked for.** List and search return metadata and
snippets; only `GET /notes/{path}` (and `get_note`) return a body, capped, with
`?section=` to slice. Every response is assumed to land in an LLM context window.

**Search is `ripgrep`, shelled out.** Fast enough to five figures of notes, can
be swapped behind the same response shape later. Diacritic folding exists
because the vault has Azerbaijani text (`sirr` must match `Şirr`).

**Go, stdlib + chi, server-rendered HTML.** Small concurrent I/O service; no
frontend framework, no build step.

## The model writes to the vault directly, one commit per note

*Was:* v0 was read-only; an interim design let the model draft into a separate
`inbox/` directory that a human had to promote by hand.
*Now:* `create_note`/`write_note`/`delete_note` write straight into the vault,
and every write is exactly one git commit touching exactly one file.
*Why:* the staging area added friction and a second place notes could live
(which fights "files are the source of truth"). Git gives the same safety more
cheaply: any bad write is one `git revert` of one commit away from gone, because
nothing is ever bundled. The vault is therefore required to be a git repo
(checked at boot, not on first write).
*Cost:* the MCP process needs a read-write mount; concurrent writers must be
serialized (`gitMu`, plus retry on git's `index.lock` across processes).

**`write_note` amends instead of stacking.** Editing one note ten times in a
session made ten commits. Now a `write_note` whose immediately-preceding commit
is an unbroken `write_note` update to the *same* note amends it. Any other commit
in between breaks the chain. `create_note`/`delete_note` never amend.

## The model has no push or commit tool

*Was:* a `push_vault` MCP tool existed briefly.
*Now:* removed; the server commits on write and never touches a remote.
*Why:* `spec/tiers.md`'s "One door" — every server-side rule (tier field, R1,
R2, the `asil` lock) is only a guarantee if the *only* path to the vault is the
API. A model with a push/commit tool could edit `tier:` in a file and commit it,
turning every rule into a convention. Publishing is the human's `git push`.

## Curation is a field (tiers), not a place

*Was:* an inbox/"lake"/"warehouse" split with promotion reports and a `curated`
boolean.
*Now:* every note has `tier: dakhil|amil|thabit|asil`; a note's folder stays
topical for life. Full design: [`spec/tiers.md`](../spec/tiers.md), which
supersedes the lake/warehouse framing entirely.
*Why:* directories encoding review status meant moving files (breaking path =
identity) and excluding uncurated notes from results lost captured knowledge.
Tiers **weight** search ranking (0.4/0.8/1.0/1.0) instead of filtering, and
`/graph` traverses every tier, marking each edge with the lower endpoint tier.

**Tier rules are enforced by code, not by asking the model.** A model told not
to raise its own tier eventually will. So: `create_note` always lands `dakhil`;
`tier_set` can only lower and *errors loudly* on a raise (a silent no-op would
teach the model the call worked); server-owned frontmatter is silently stripped
from model writes; R2 (editing `thabit` demotes to `amil`) happens inside the
write, atomically; `asil` is also `0444` on disk so even a bug in the app-layer
check hits a permission error.

**No `tier_expire` tool.** Stale `dakhil` notes are a query (`?tier=dakhil&
updated_before=…`) that Al-Mina surfaces after `NULL_MINA_STALE_DAYS` (14).

## The web UI now writes (second reversal of "read-only")

*Was:* `nullapi` and the renderer were strictly read-only (mount `:ro`).
*Now:* the renderer performs exactly two writes, both tier metadata, one commit
each, through `internal/vault/human.go`: promote `dakhil`→`amil` when the human
first opens a note, and Al-Mina's approve/deny/defer. The JSON API still never
writes.
*Why:* the tier spec makes raising a tier "a human act", and the only surface
that can be unambiguously human is the browser. Promotion needs *some* code path
the model cannot reach; `human.go` is referenced only from `internal/render`,
enforced by a test that scans the source.
*Cost — and the mitigations that are non-negotiable:* the renderer serves
model-authored HTML (note bodies) *and* can now write, so a script or form
planted in a note could otherwise forge an approval or a "human read". Hence
CSP on every page, a CSRF token on write forms, a same-origin check on POSTs,
and first-open promotion counting only a genuine user navigation
(`Sec-Fetch-User: ?1`, `Sec-Fetch-Dest: document`, cookie session). The vault
mount for `nullapi` became read-write.

**Two tokens.** `NULL_TOKEN` gates the API/MCP; `NULL_UI_TOKEN` gates the
browser, Al-Mina and `/setup`. With one shared token, a program holding the API
token could log in to the UI and approve its own proposals, defeating the
"model cannot forge a human read" property. They default to equal (so a
single-user local setup just works) but the server warns at boot.

## Al-Mina lives at `/al-mina` (and `/mina` is JSON)

`spec/tiers.md` names `GET /mina` as the queue endpoint and calls Al-Mina a
renderer view. The bearer-authenticated JSON queue kept `/mina`; the cookie-
authenticated review screen is `/al-mina` (POST `/al-mina/act`). Both are the
same query (`vault.MinaQueue`). Decisions are batch-applied but committed one
note at a time. "What changed since last review" is a boolean derived from
`tier_history`, not a diff — a real diff would shell out to `git log -p` per
queue entry.

## The graph view is hand-written

`spec/tiers.md` moved graph visualization into scope and allowed "one
force-directed library". We wrote ~200 lines of canvas + a grid-accelerated
force layout instead: no vendoring, no licence to carry, no build step, and the
tier-specific drawing (dashed `dakhil` edges, colour per tier) is trivial in
hand-written code. It is one of only two JS files (`graph.js`, `mina.js`),
embedded and CSP-clean. Lesson learned: the first version could diverge to
`Infinity` and freeze the tab (the grid loop never terminates on a non-finite
coordinate); repulsion is now softened, velocity is capped, and non-finite
positions are reset. Keep that guard.

## Choosing the vault in the browser (`/setup`)

*Why:* open-sourcing means strangers who should not have to edit env vars to
point at a folder.
*Shape:* `NULL_VAULT_PATH` (env) wins and fixes the vault (`/setup` read-only);
else the saved config file; else **setup mode** (only `/login` and `/setup`
respond). Picking a folder hot-swaps index/watcher/API/renderer without a
restart (`app.Manager.Start`) and saves the choice atomically, mode `0600`.
`nullmcp` reads the same file at boot.
*The risk is filesystem access from a web request*, so the picker can only see
inside `NULL_BROWSE_ROOT`, with symlinks resolved before the containment check,
dot-directories refused and only real directories listed. A browse root is only
required when the vault is chosen in the browser (a fixed-vault deployment that
never mounts one must still boot — a regression we shipped once and fixed).

## OAuth on the MCP HTTP transport

Claude.ai's connector requires the MCP authorization flow (OAuth 2.1 + dynamic
client registration, PKCE S256, resource indicators), not a pasted header. It
is implemented minimally and is a protocol envelope around the *one* existing
secret (`NULL_TOKEN`) — there are still no users or roles. stdio needs no auth
(the OS process boundary is the boundary); HTTP mode is mutually exclusive with
stdio in one process, because a daemon's stdin hits EOF and would shut a stdio
server down.

**OAuth state is in memory by default, persistable by opt-in.** A restart
forgets registered clients; a client with a stale registration then hits a
plain `400 unknown client_id` at `/authorize` (deliberately no redirect — that
would be an open redirect) and can stay stuck until its connector is removed
and re-added. Fine for a long-lived server, painful for a laptop restarted
constantly, so `NULL_MCP_STATE_PATH` persists clients and tokens (one small
`0600` JSON file, written atomically via `config.WriteJSON`). Tokens are stored
only as SHA-256 hashes (they are 256-bit random, so the hash both validates
and is useless to steal); authorization codes are never stored; a file written
for another origin is discarded, since tokens are bound to the resource URI
anyway. It is opt-in so container deployments with a read-only root are
unchanged. Not a database — non-negotiable #1 stands.

## Deliberately not built

Embeddings/RAG, users and roles, rate limiting, in-browser editing of note
*bodies*, a JS build step, automatic pushing, any MCP tool that raises a tier.
Each is a real future need; none can be specified before the vault has been used
for a while. The renderer stays a reader with one narrow, structured exception
(the two human tier actions).

## Traps we already fell into

- **Unanchored ignore patterns.** `.gitignore` containing `nullapi` also
  ignored the `cmd/nullapi/` directory, silently untracking the entrypoint for
  weeks; `tar --exclude` had the same behaviour. Anchor to the root (`/nullapi`).
- **Metadata-only rewrites must be byte-stable.** `serialize` puts one blank line
  under the frontmatter fence and `Parse` keeps it as the body's first byte;
  rewriting frontmatter without stripping it grew the body by a blank line every
  time. `rewriteBody` fixes it; `TestMetadataRewritesDoNotGrowTheBody` guards it.
- **Timestamp precision.** `denied_at` round-trips through RFC 3339 (whole
  seconds) while file mtimes are nanoseconds, so "edited since the denial?" needs
  truncation plus a small slack, or a proposal could be resubmitted in the same
  instant it was denied.
- **Git in containers.** A container user rarely owns a bind-mounted vault, so
  every git call passes `-c safe.directory=*`, and commit identity comes from
  `GIT_*` environment variables, not on-disk config.
- **Test the deployment shapes you ship.** The `/vaults` regression above passed
  every unit test because only the setup-page layout had been run end to end.
- **Label collision.** `spec/tiers.md`'s "M6a–M7" are unrelated to the archived
  build plan's M6/M7 (see [`history/build-plan-v0.md`](history/build-plan-v0.md)).
