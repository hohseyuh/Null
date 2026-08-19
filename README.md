# null-service

Read-only HTTP lens over a plain-markdown vault. JSON API plus a
server-rendered HTML reader, one process, one in-memory index. The vault
stays canonical on disk; delete this server and nothing is lost.

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

Bare metal: `deploy/nullapi.service` (systemd, `DynamicUser`,
`ReadOnlyPaths=` on the vault). Terminate TLS in your reverse proxy;
the service listens on loopback.

Vault sync is `git pull` in the vault clone (cron or push webhook) — the
fsnotify watcher picks the changes up, batched behind a debounce. No
reload endpoint exists because none is needed.
