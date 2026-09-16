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
HTTP — including writing to it. Eleven tools: `list_notes`, `get_note`,
`search_notes`, `get_graph` mirror the HTTP routes; `find_relatives`
(folder/tag siblings), `get_links` (one note's outlinks+backlinks in one
call), and `find_path` (shortest link chain between two notes) are new,
read-only, no HTTP equivalent; `create_note`, `write_note`, `delete_note`,
and `push_vault` are the write path — always available, no configuration
flag. Full contract in [`spec/null-mcp-v0.md`](spec/null-mcp-v0.md).

**`NULL_VAULT_PATH` must already be a git repository** — checked at boot.
Every `create_note`/`write_note`/`delete_note` call becomes exactly one
git commit, touching exactly one file: `"Add <path>"`, `"Update <path>"`,
`"Delete <path>"`, plus an optional `reason` field appended to the
message. That's the whole safety model — a bad write is one `git revert`
away from gone, never entangled with anything else, because there's
never more than one change per commit. `push_vault` is a separate,
explicit tool; nothing reaches the remote until it's called on purpose.

```sh
NULL_VAULT_PATH=/srv/null-vault go run ./cmd/nullmcp
```

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
alongside `nullapi`, mounting the *same* `VAULT_PATH` but read-write
(nullapi's mount stays `:ro`; nullmcp's does not — see "MCP server"
above). `VAULT_PATH` must already be a git repository before you start
this service — `git init && git add -A && git commit -m seed`, if it
isn't one yet:

```sh
echo "NULL_MCP_TOKEN=$(openssl rand -hex 32)" >> .env   # separate from NULL_TOKEN, deliberately
echo "NULL_MCP_PUBLIC_URL=https://your-host:10000" >> .env
docker compose up -d --build
```

Bare metal: `deploy/nullapi.service` (systemd, `DynamicUser`,
`ReadOnlyPaths=` on the vault). Terminate TLS in your reverse proxy;
the service listens on loopback.

Vault sync is `git pull` in the vault clone (cron or push webhook) — the
fsnotify watcher picks the changes up, batched behind a debounce. No
reload endpoint exists because none is needed.
