# null-service

Read-only lens over a plain-markdown vault, served three ways from the
same in-memory index: a JSON API, a server-rendered HTML reader, and an
MCP server for a language model to call directly. The vault stays
canonical on disk; delete this server and nothing is lost.

## Requirements

- Go 1.22+
- `ripgrep` on PATH (`/search` shells out to it; boot fails loudly without it)

## Configuration (env only)

| var | required | default | meaning |
|---|---|---|---|
| `NULL_VAULT_PATH` | yes | — | vault root directory |
| `NULL_TOKEN` | yes | — | static bearer token |
| `NULL_ADDR` | no | `:8080` | listen address |
| `NULL_MAX_BODY_BYTES` | no | `200000` | single-note body cap; larger bodies 413 and point at `?section=` |

## Run

```sh
NULL_VAULT_PATH=/srv/null-vault NULL_TOKEN=$(openssl rand -hex 32) go run ./cmd/nullapi
```

## Routes

All `/v1` routes except `/v1/health` require `Authorization: Bearer $NULL_TOKEN`.

```sh
# health — the only open route
curl http://localhost:8080/v1/health

# list notes: metadata only, never bodies
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/notes?folder=engineering/&tag=spec&sort=updated&limit=50'

# one note — the only route that returns a body; slice big notes by heading
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/notes/engineering/basim/soul.md?include=body,outlinks,backlinks'
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/notes/engineering/basim/soul.md?section=Failure%20modes'

# search: snippets, never bodies; case- and diacritic-insensitive (sirr matches Şirr)
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/search?q=sirr&in=body&limit=20'

# graph: BFS neighborhood; every edge carries the line its wikilink was written on
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/graph?path=engineering/basim/soul.md&depth=2&direction=both'
```

### HTML reader

Same token, held by a cookie: open `/login?token=$NULL_TOKEN` once, then

- `/` — note list grouped by folder
- `/n/{path}` — the note; wikilinks are clickable, backlinks listed with context
- `/s?q=` — search with highlighted snippets

No editor, no JS, one stylesheet.

### MCP server

For a language model to call the vault directly instead of going through
HTTP. Eleven tools: `list_notes`, `get_note`, `search_notes`, `get_graph`
mirror the HTTP routes; `find_relatives` (folder/tag siblings),
`get_links` (one note's outlinks+backlinks in one call), and `find_path`
(shortest link chain between two notes) are new, read-only, no HTTP
equivalent. `get_graph`/`find_path` include inbox content when an inbox
is configured (a query can be rooted at, or end at, a draft);
`get_graph_vault_only`/`find_path_vault_only` are always-available
companions that guarantee the opposite — a settled-vault-only view no
draft can ever slip into, regardless of configuration.
`create_note`/`write_note` are writes, and only exist at all when
`NULL_INBOX_PATH` is configured. Full contract in
[`spec/null-mcp-v0.md`](spec/null-mcp-v0.md).

```sh
NULL_VAULT_PATH=/srv/null-vault go run ./cmd/nullmcp
```

#### Inbox (optional — enables create_note/write_note)

```sh
NULL_VAULT_PATH=/srv/null-vault NULL_INBOX_PATH=/srv/null-inbox go run ./cmd/nullmcp
```

`NULL_INBOX_PATH` is a second, physically separate directory — never part
of the `null-vault` git repo. It's the *only* thing any write tool ever
opens read-write; `NULL_VAULT_PATH` stays exactly as read-only as it is
in `nullapi`, unconditionally. Notes written there show up immediately
(no restart, no race against the watcher) in every read tool, addressed
as `inbox/<path>` and labeled — same shape as any other note, just with
`source: "inbox"` and a `[inbox — draft, not yet reviewed or promoted]`
suffix on the title, so a model can't mistake a draft for settled fact.

Promoting a draft into the real vault is a human act, on purpose: review
it, move it into `null-vault`, `git add && commit && push` yourself. No
tool here does that step for you — see CLAUDE.md's "Inbox" section for
why, and `spec/null-mcp-v0.md` for the one real limitation this has today
(links between a draft and a real note don't appear as graph edges until
the draft is promoted).

Speaks stdio — point a client (Claude Desktop, Claude Code, etc.) at the
binary as a subprocess, e.g. in Claude Desktop's config:

```json
{
  "mcpServers": {
    "null": {
      "command": "/path/to/nullmcp",
      "env": { "NULL_VAULT_PATH": "/srv/null-vault" }
    }
  }
}
```

No `NULL_TOKEN` needed here — stdio's trust boundary is the OS process
that spawns the binary.

#### Remote clients (e.g. Claude.ai's connector settings)

For a client that can't spawn a local subprocess, set `NULL_MCP_HTTP_ADDR`
to switch to the Streamable HTTP transport instead of stdio — never both
in the same process; see `spec/null-mcp-v0.md`'s "HTTP transport" section
for why. `NULL_TOKEN` and `NULL_MCP_PUBLIC_URL` (this server's own public
HTTPS origin — needed for OAuth discovery, see below) are then required:

```sh
NULL_VAULT_PATH=/srv/null-vault NULL_MCP_HTTP_ADDR=127.0.0.1:8092 \
  NULL_TOKEN=$(openssl rand -hex 32) \
  NULL_MCP_PUBLIC_URL=https://your-host:10000 go run ./cmd/nullmcp
```

Two ways to authenticate against `https://your-host:10000/mcp`, both
described in full in `spec/null-mcp-v0.md`'s OAuth section:

- **A plain client** (curl, testing, anything that doesn't need OAuth):
  `Authorization: Bearer <NULL_TOKEN>`, directly.
- **Claude.ai's connector settings**, or any MCP-authorization-spec-
  compliant client: it self-registers and discovers the flow on its own
  (`.well-known/oauth-protected-resource` → `.well-known/oauth-
  authorization-server` → `/register` → `/authorize` → `/token`) — just
  give it the `/mcp` URL. The one thing you'll do by hand is type
  `NULL_TOKEN` into the `/authorize` page's form once, in the browser,
  when the client opens it.

This process only ever binds loopback; a reverse proxy (this repo's own
deployment uses Tailscale Funnel) is what makes it reachable from
anywhere else, and terminates TLS. This binary never does.

To poke at the tools by hand during development, set
`NULL_MCP_INSPECTOR_ADDR=127.0.0.1:8090` and open that address — a
server-rendered, zero-JS page that calls tools over a real MCP session.
Dev-only, no auth, keep it on loopback.

## Tests

```sh
go test ./...            # needs ripgrep installed
go test ./... -race      # watcher concurrency
```

## Deploy

Docker (vault mounted read-only, enforced twice — `:ro` mount and
`O_RDONLY`-only opens in code):

```sh
echo "NULL_TOKEN=$(openssl rand -hex 32)" > .env
echo "VAULT_PATH=/srv/null-vault" >> .env
docker compose up -d --build

# verify the mount really is read-only
docker compose exec nullapi sh -c 'mount | grep vault && touch /vault/x; echo exit=$?'
# expect: ...(ro,...) and "Read-only file system", exit=1
```

`compose.yaml` also has an `nullmcp` service (same image, different
entrypoint) for the HTTP transport — add to `.env` and it starts
alongside `nullapi`:

```sh
echo "NULL_MCP_TOKEN=$(openssl rand -hex 32)" >> .env   # separate from NULL_TOKEN, deliberately
echo "INBOX_PATH=/srv/null-inbox" >> .env
docker compose up -d --build
```

Bare metal: `deploy/nullapi.service` (systemd, `DynamicUser`,
`ReadOnlyPaths=` on the vault). Terminate TLS in your reverse proxy;
the service listens on loopback.

Vault sync is `git pull` in the vault clone (cron or push webhook) — the
fsnotify watcher picks the changes up, batched behind a debounce. No
reload endpoint exists because none is needed.
