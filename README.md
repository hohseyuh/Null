# Null

A plain-markdown knowledge vault, served three ways from one in-memory
index: a **JSON API**, a **server-rendered web reader** (with a graph view
and a review screen), and an **MCP server** that lets a language model read
*and write* the vault. The vault stays plain files under git — delete this
server and nothing is lost.

What makes it different from "a notes API" is the trust model. Every note
carries a **curation tier** — `dakhil` (model-written, unseen), `amil`
(you've opened it), `thabit` (you reviewed it), `asil` (foundational) — and
the rules about it are enforced by the server, not requested of the model:

- the model can **never raise a tier**; it can only lower one or *propose* a
  promotion for you to accept or reject in a review screen called **Al-Mina**;
- editing a `thabit` note demotes it to `amil` in the same commit;
- an `asil` note is read-only to the model, and write-locked (`0444`) on disk;
- every model write is exactly **one git commit for one note**, so any bad
  write is one `git revert` away from gone;
- the model gets **no push or commit tool at all**.

## Documentation map

| Read | For |
|---|---|
| this README | what it is, running it, configuration |
| [`docs/architecture.md`](docs/architecture.md) | how it works, who can do what, routes, env vars, and which test enforces each guarantee |
| [`docs/decisions.md`](docs/decisions.md) | why it is built this way, including every deliberate reversal |
| [`spec/tiers.md`](spec/tiers.md) | the curation-tier design (canonical) |
| [`spec/null-read-api-v0.md`](spec/null-read-api-v0.md) · [`spec/null-mcp-v0.md`](spec/null-mcp-v0.md) | the HTTP API and MCP contracts |
| [`CLAUDE.md`](CLAUDE.md) | rules and map for contributors and coding agents (start here if you are a model) |
| [`SECURITY.md`](SECURITY.md) · [`CONTRIBUTING.md`](CONTRIBUTING.md) | threat model / reporting · how to contribute |

## Quick start

You need [Go](https://go.dev) 1.22+, [`ripgrep`](https://github.com/BurntSushi/ripgrep)
and `git` on your PATH.

```sh
git clone <this repo> && cd null-service

# two secrets: the API token, and a separate one for the browser UI
export NULL_TOKEN=$(openssl rand -hex 32)
export NULL_UI_TOKEN=$(openssl rand -hex 32)

go run ./cmd/nullapi
```

Open `http://localhost:8080/login?token=<NULL_UI_TOKEN>` once, then you are
sent to **`/setup`**: pick the folder that holds your notes (browsing is
limited to your home directory; change that with `NULL_BROWSE_ROOT`). The
choice is remembered in a config file and applied immediately — no restart.
Or skip the page and set `NULL_VAULT_PATH=/path/to/vault`, which fixes the
vault and makes `/setup` read-only.

The vault should be a git repository (`git init` in it) — reading works
either way, but every write (the model's, and your Al-Mina decisions) is a
commit, so they need one.

Try it with the bundled sample vault: `NULL_VAULT_PATH=$PWD/testdata/vault`.

## Configuration

Everything is environment variables.

| var | default | meaning |
|---|---|---|
| `NULL_TOKEN` | — (required) | bearer token for the JSON API |
| `NULL_UI_TOKEN` | `NULL_TOKEN` | token for the browser UI, Al-Mina and `/setup`. **Set it separately** if any program or model holds the API token — otherwise that program can log in and approve its own proposals. A boot warning tells you when it is unset. |
| `NULL_VAULT_PATH` | unset | vault root. Set: fixed, `/setup` read-only. Unset: the saved choice, else setup mode |
| `NULL_CONFIG_PATH` | user config dir `/null/config.json` | where `/setup` saves the choice (mode 0600) |
| `NULL_BROWSE_ROOT` | `$HOME` | the only area `/setup` may browse |
| `NULL_ADDR` | `:8080` | listen address |
| `NULL_MAX_BODY_BYTES` | `200000` | single-note body cap; larger bodies 413 and point at `?section=` |
| `NULL_MINA_STALE_DAYS` | `14` | age at which an untouched `dakhil` note surfaces in Al-Mina |
| `NULL_GIT_NAME` / `NULL_GIT_EMAIL` | binary name / `<name>@localhost` | author of commits the server makes |

Nothing terminates TLS: put a reverse proxy in front for anything beyond
localhost.

## The web reader

Log in once with `/login?token=<NULL_UI_TOKEN>` (sets an HttpOnly cookie).

- `/` — every note grouped by folder, tier dot on each; `?tier=amil` filters
- `/n/{path}` — the note; wikilinks clickable, backlinks with context, tier history
- `/s?q=` — search with highlighted snippets
- `/graph` — the whole vault as a force-directed graph. Colour is the tier;
  edges touching a `dakhil` note are dashed (an edge is only as trustworthy as
  its weaker end). Scroll to zoom, drag to pan, click a node to open it.
- `/al-mina` — the port: the model's tier proposals with its reason inline,
  plus stale `dakhil` notes. Approve / deny / defer in one batch; keyboard-driven
  (`j`/`k` move, `a` `d` `f` `s` decide, `Enter` applies). Works without
  JavaScript too.
- The header always shows how many notes sit at each tier, and how many items
  wait in Al-Mina.

Opening a `dakhil` note in the browser marks it `amil` — your reading is the
evidence tier 2 claims. Only a real, user-initiated page load counts; an API
call, a prefetch, or an image tag inside a note cannot.

The only JavaScript is two small hand-written files (`graph.js`, `mina.js`).
There is no build step.

## The JSON API

All `/v1` routes except `/v1/health` need `Authorization: Bearer $NULL_TOKEN`.
Responses are compact JSON, and every note entry carries its `tier`.

```sh
# list notes: metadata only, never bodies
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/notes?folder=engineering/&tag=spec&sort=updated&limit=50'
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/notes?tier=dakhil&updated_before=2026-01-01T00:00:00Z'

# one note — the only route that returns a body; slice big notes by heading
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/notes/engineering/basim/soul.md?include=body,outlinks,backlinks'
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/notes/engineering/basim/soul.md?section=Failure%20modes'

# search: snippets, never bodies; case- and diacritic-insensitive (sirr matches Şirr).
# Scores are weighted by tier — raw capture is ranked below reviewed notes, not hidden.
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/search?q=sirr&in=body&limit=20'

# graph: BFS neighborhood; each edge has the line its wikilink was written on
# and the lower of its two endpoints' tiers
curl -H "Authorization: Bearer $NULL_TOKEN" \
  'http://localhost:8080/v1/graph?path=engineering/basim/soul.md&depth=2&direction=both'

# Al-Mina's queue as JSON (read-only; deciding happens in the browser)
curl -H "Authorization: Bearer $NULL_TOKEN" 'http://localhost:8080/mina'
```

The API never writes. Contract: [`spec/null-read-api-v0.md`](spec/null-read-api-v0.md).

## The MCP server

For a language model to use the vault directly. Thirteen tools:
`list_notes`, `get_note`, `search_notes`, `get_graph`, `find_relatives`,
`get_links`, `find_path` (read); `create_note`, `write_note`, `delete_note`
(write); `tier_get`, `tier_set` (lowering only), `tier_propose`. Full
contract: [`spec/null-mcp-v0.md`](spec/null-mcp-v0.md).

`nullmcp` needs the vault to be a git repository (checked at boot). It reads
the same saved vault choice as `nullapi`, or `NULL_VAULT_PATH`.

```sh
NULL_VAULT_PATH=/path/to/vault go run ./cmd/nullmcp      # stdio
```

Point a client such as Claude Desktop at the binary:

```json
{ "mcpServers": { "null": {
    "command": "/path/to/nullmcp",
    "env": { "NULL_VAULT_PATH": "/path/to/vault" } } } }
```

**Remote clients** (e.g. Claude.ai's connector settings) can't spawn a
process, so `NULL_MCP_HTTP_ADDR` switches to Streamable HTTP with OAuth 2.1 +
dynamic client registration wrapped around one secret. It needs `NULL_TOKEN`
and `NULL_MCP_PUBLIC_URL` (the public https origin your reverse proxy exposes):

```sh
NULL_VAULT_PATH=/path/to/vault NULL_MCP_HTTP_ADDR=127.0.0.1:8092 \
  NULL_TOKEN=$(openssl rand -hex 32) NULL_MCP_PUBLIC_URL=https://your-host \
  go run ./cmd/nullmcp
```

Give the client `https://your-host/mcp`; it discovers the rest and asks you to
type the token once, in the browser. Use a *different* token from
`NULL_UI_TOKEN`. For poking at tools by hand, `NULL_MCP_INSPECTOR_ADDR=127.0.0.1:8090`
serves a dev-only page (no auth — loopback only).

There is deliberately **no push or commit tool**. The server commits on every
write; you push with your own `git push` when you choose.

## Deploy

Docker, vault fixed by the environment:

```sh
cp .env.example .env    # then fill in the secrets
docker compose up -d --build
```

Or choose the vault in the browser (`compose.setup.yaml`, see its header).
Both mount the vault **read-write** — the model's writes and your Al-Mina
decisions are git commits made inside the container. The vault directory
must be a git repository. Bare metal: `deploy/nullapi.service` (systemd).

Vault sync from your own machine is `git pull` in the vault clone; the
file watcher notices and re-indexes.

## Development

```sh
go build ./... && go vet ./...
go test ./... -race     # needs ripgrep and git
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Security-relevant behaviour and how
to report a problem: [`SECURITY.md`](SECURITY.md).

## License

[MIT](LICENSE).
