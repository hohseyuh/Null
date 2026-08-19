# Null — Read API v0

Read-only HTTP layer over the vault. Files stay the source of truth; this is a lens, not a store.

**Base:** `https://null.<host>/v1`
**Auth:** `Authorization: Bearer <token>` on every route. One static token, rotated by hand. It is your VPS — do not build OAuth for a single user.
**Errors:** `{ "error": "code", "detail": "..." }` with the obvious status codes.

---

## Design rules

**Path is the ID.** No UUIDs, no database keys. `engineering/basim/soul.md` identifies the note everywhere. If you ever invent an ID that doesn't map to a filepath, you've started building the prison.

**Never return a body you weren't asked for.** List and search return metadata only. Bodies come from `GET /notes/{path}` alone. This is the single most important rule — it is what keeps Basim's context window from drowning when the vault hits 2,000 notes.

**Frontmatter parsed, body raw.** Return YAML frontmatter as a JSON object and the body as unmodified markdown. Do not render, do not resolve wikilinks inline, do not strip anything. Basim reads markdown natively.

**Everything is UTF-8 and path-normalized.** Reject `..`, absolute paths, and anything outside the vault root before touching disk.

---

## 1. `GET /notes`

List notes. Metadata only.

**Query params**
| param | type | notes |
|---|---|---|
| `folder` | string | prefix filter, e.g. `engineering/` |
| `tag` | string | repeatable; AND semantics |
| `updated_after` | ISO 8601 | mtime filter |
| `limit` | int | default 50, max 200 |
| `cursor` | string | opaque; last path of previous page |
| `sort` | enum | `path` \| `updated` (default `updated` desc) |

**Response**
```json
{
  "notes": [
    {
      "path": "engineering/basim/soul.md",
      "title": "soul",
      "tags": ["basim", "spec"],
      "frontmatter": { "status": "active", "type": "spec" },
      "updated_at": "2026-08-19T09:14:02Z",
      "size_bytes": 4820,
      "outlink_count": 3,
      "backlink_count": 7
    }
  ],
  "next_cursor": "engineering/basim/soul.md"
}
```

---

## 2. `GET /notes/{path}`

Fetch one note. The only route that returns a body.

**Query params**
| param | type | notes |
|---|---|---|
| `section` | string | heading text, e.g. `Failure modes`. Returns only that heading and its content, to the next same-or-higher heading. |
| `include` | csv | `body` (default), `outlinks`, `backlinks` |

`section` matters more than it looks. It is what lets Basim pull one clause out of a long note instead of the whole thing — and it's the same addressing scheme the write API will use later to append without clobbering. Build it now even though nothing needs it yet.

**Response**
```json
{
  "path": "engineering/basim/soul.md",
  "frontmatter": { "status": "active", "type": "spec", "tags": ["basim"] },
  "body": "# soul.md\n\n> The layer that does not change...",
  "headings": [
    { "text": "Who I am", "level": 2, "line": 8 },
    { "text": "Failure modes", "level": 2, "line": 96 }
  ],
  "outlinks": ["engineering/basim/character.md"],
  "backlinks": ["engineering/basim/index.md"],
  "updated_at": "2026-08-19T09:14:02Z"
}
```

`404` if absent. `413` if the body exceeds a configured cap — force the caller to use `section`.

---

## 3. `GET /search`

**Query params**
| param | type | notes |
|---|---|---|
| `q` | string | required; case- and diacritic-insensitive substring for now |
| `in` | enum | `body` (default) \| `title` \| `both` |
| `folder`, `tag` | | same as `/notes` |
| `limit` | int | default 20, max 50 |

**Response — snippets, never bodies**
```json
{
  "results": [
    {
      "path": "philosophy/barzakh.md",
      "title": "barzakh",
      "score": 0.82,
      "matches": [
        { "line": 14, "snippet": "...the interval between states, unresolved..." }
      ]
    }
  ]
}
```

**Start with `ripgrep` shelled out over the vault.** Genuinely. It is fast enough to five figures of notes and you can swap in BM25, then embeddings, behind this exact response shape without the caller noticing. Do not build a vector store in week one — you do not yet know what you need to retrieve.

---

## 4. `GET /graph`

The route that justifies markdown over a database.

**Query params**
| param | type | notes |
|---|---|---|
| `path` | string | required; the anchor note |
| `depth` | int | default 1, max 3 |
| `direction` | enum | `out` \| `in` \| `both` (default) |

**Response**
```json
{
  "root": "engineering/basim/soul.md",
  "nodes": [
    { "path": "engineering/basim/character.md", "title": "character", "distance": 1 }
  ],
  "edges": [
    {
      "from": "engineering/basim/soul.md",
      "to": "engineering/basim/character.md",
      "context": "If character.md and this file ever disagree, this file wins."
    }
  ]
}
```

The `context` field — the line the wikilink appeared on — is the whole prize. Embedding search tells you two notes are *near*. This tells you *why they were connected*, in your own words, at the time you connected them. No vector store recovers that.

---

## Implementation notes

**Index in memory, watch the disk.** Parse the vault into a dict on boot, `inotify`/`fswatch` for changes, re-parse only the touched file. A few thousand notes is a few MB — you do not need SQLite until you do.

**Cache invalidation is mtime.** Nothing cleverer.

**Log every request with path and byte count.** This is the log from step 4 of the plan — where Basim looked, what he got back, how big it was. It is how you learn your schema instead of guessing it.

**Rate-limit nothing. Auth once. Ship it.**

---

## Deliberately absent

- Writes, of any kind
- In-browser editing (the HTML renderer is a separate, read-only concern — see BUILD_PLAN M6)
- Embeddings, RAG, chunking
- Users, roles, sharing
- Anything touching dotfiles or dot-directories, `.git/` above all

Each of these is a real thing you will want later. None is a thing you can specify correctly today.
