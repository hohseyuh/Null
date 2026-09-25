# Build plan — Null Read API v0 (archived)

Ordered milestones. Each ends at a state you can run and verify. Do not start one before the previous passes.

Read `../../spec/null-read-api-v0.md` before M1 and re-read the relevant section at the start of each milestone.

> **ARCHIVED — do not use as a plan or a checklist.** This is the original
> milestone plan for the *read-only* API (M0–M7), kept because it explains why
> the early code is shaped as it is. All of it shipped, and several of its
> premises were later **deliberately reversed**: the vault is no longer
> read-only or mounted `:ro`, JavaScript is no longer "none", and there is a
> curation-tier system and a setup page it never anticipated. The
> "Verification checklist" at the bottom is therefore *stale*. For the current
> system read [`../architecture.md`](../architecture.md) and, for why things
> changed, [`../decisions.md`](../decisions.md). Note that `spec/tiers.md`
> reuses the labels M6a/M6b/M6c/M7 for an unrelated, later sequence.

---

## M0 — Skeleton

- `go mod init`, chi router, `GET /v1/health` returning `{"status":"ok","notes_indexed":0}`.
- Config from env: `NULL_VAULT_PATH`, `NULL_TOKEN`, `NULL_ADDR`, `NULL_MAX_BODY_BYTES` (default 200_000).
- Bearer auth middleware on everything except `/v1/health`. Constant-time compare.
- Structured request logging: method, path, status, duration, response bytes.
- Graceful shutdown on SIGTERM.

**Done when:** server boots, health responds, a wrong token gets `401`.

---

## M1 — Parse

`internal/vault/note.go`. No HTTP in this milestone.

- Parse one file into `Note{Path, Frontmatter map[string]any, Body string, Headings []Heading, Outlinks []Link}`.
- Frontmatter: YAML between leading `---` fences. Absent frontmatter is valid, not an error. Malformed YAML logs a warning and yields an empty map — **never fails the whole index**.
- Headings via goldmark AST: text, level, line number.
- Wikilinks: `[[target]]`, `[[target|alias]]`, `[[target#section]]`. Capture the **full line** each link appeared on — this becomes the `context` field in `/graph` and is the point of the whole route.
- Resolve link targets to paths: exact path match, else basename match, else leave unresolved and mark it. Unresolved links are normal, not errors.

**Done when:** table tests pass over `testdata/vault/`, including the unicode note, the frontmatter-less note, and the broken link.

---

## M2 — Index

`internal/vault/index.go`.

- Walk the vault, skip excluded dirs, build `map[string]*Note`.
- Build the reverse edge map for backlinks in the same pass.
- `RWMutex` around it. Reads are the hot path.
- Boot timing logged. If a few thousand notes takes more than a second or two, say so before optimizing anything.

`internal/vault/watcher.go`:
- `fsnotify` on the vault, recursive. Debounce ~200ms — editors write in bursts.
- On change: reparse that file only, patch the index and both edge maps. On delete: remove and clean dangling edges.

**Done when:** editing a note on disk changes the index without a restart, verified by a test that writes to a temp vault — and a second test that changes fifty files at once (simulating a `git pull`) and confirms the index settles to the correct state with a bounded number of reparses.

---

## M3 — `/notes` and `/notes/{path}`

- `GET /v1/notes` — folder/tag/updated_after filters, cursor pagination, sort. **Metadata only.** Assert this in a test that fails if `body` appears anywhere in the response.
- `GET /v1/notes/{path...}` — full note. `?include=` for outlinks/backlinks. `?section=<heading>` slices from that heading to the next same-or-higher one; unknown heading → `404`.
- Body over `NULL_MAX_BODY_BYTES` → `413` with a detail telling the caller to use `?section=`.
- Path safety function wired in, with adversarial tests: `../../etc/passwd`, `/etc/passwd`, url-encoded traversal, a symlink pointing outside root.

**Done when:** both routes match the spec's example payloads field-for-field.

---

## M4 — `/search`

- Shell out to `rg --json --smart-case --type md`, parse the JSON stream.
- Return path, title, score, and line-numbered snippets. **Never bodies.**
- `folder`/`tag` filters applied by intersecting with the index after rg returns.
- Diacritic folding for `ə ğ ı ö ş ü ç` so `sirr` matches `şirr`. Azerbaijani notes make this non-optional.
- Timeout the subprocess at 5s. Missing `rg` binary → clear startup error, not a runtime `500`.

**Done when:** search returns hits over the fixture vault and a body never appears in a response.

---

## M5 — `/graph`

- Traverse out/in/both to `depth` (max 3), BFS, dedup by path, cycle-safe.
- Each edge carries the `context` line captured in M1.
- Depth > 3 → `400`.

**Done when:** a two-hop query on the fixture vault returns the expected node and edge sets, with context lines present.

---

## M6 — Renderer

Same process, same index, no HTTP calls to your own API. `internal/render/`.

- `GET /` — note list grouped by folder, sorted by updated. Title, tags, date.
- `GET /n/{path...}` — the note. goldmark → HTML. Frontmatter as a small header block, not raw YAML. Backlinks listed at the bottom with their context lines.
- `GET /s?q=` — search results, snippets with the match highlighted.
- **Wikilink rewriting is the whole point.** `[[target]]` → `<a href="/n/{resolved}">`. Unresolved links render as a distinct dead-link style, not as broken HTML and not as plain text.
- Auth: same bearer token, accepted as a cookie set by a `GET /login?token=` route so a browser can hold it. Do not build a login form.
- One hand-written stylesheet. Readable measure (~70ch), generous line height, one font stack, dark by default. No framework, no bundler, no JS beyond none.

**Done when:** you can read a note in a browser and click a wikilink to reach the next one.

---

## M7 — Ship

- `Dockerfile`, multi-stage, vault mounted read-only (`:ro`) — belt and braces with the `O_RDONLY` rule.
- systemd unit or compose file, restart on failure.
- `README.md`: env vars, one curl example per route, how to run tests.
- Verify: `mount | grep vault` shows `ro`, and a write attempt from inside the container fails.

**Done when:** it runs on the VPS behind TLS and survives a reboot.

---

## After M7 — do not skip this

Point Basim v0 at it. Use it daily for two weeks. **Log every retrieval miss** — what he searched for, what came back, what you had to fetch by hand.

That log is the input to the frontmatter schema and the write policy. Both are currently unspecifiable, and guessing at them now buys a migration later.

---

## Verification checklist (stale — see the banner above)

- [ ] No route writes to disk
- [ ] `/notes` and `/search` never return a body field
- [ ] Traversal tests pass
- [ ] Malformed frontmatter degrades, never crashes the index
- [ ] Watcher survives rapid edits and file deletion
- [ ] Unauthenticated request to every route → `401`
- [ ] Boot time and index size logged
- [ ] Renderer reads the index directly — grep the codebase for HTTP calls to self, there should be none
- [ ] Wikilinks in rendered notes are clickable and resolve
- [ ] Zero JS files shipped, zero build step in the Dockerfile
