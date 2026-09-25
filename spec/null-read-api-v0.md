# Null — Read API v0

Read-only HTTP layer over the vault. Files stay the source of truth; this is a lens, not a store.

> **Status: implemented and current**, with additions since the first draft
> that are marked **(tiers)** below: every note entry carries its curation
> `tier`, `tier`/`updated_before` filters exist, search scores are tier-weighted,
> graph nodes/edges carry tiers, and `GET /mina` exists. The API itself still
> never writes — the write paths (MCP tools; the browser's two tier actions) are
> documented in [`null-mcp-v0.md`](null-mcp-v0.md) and
> [`../docs/architecture.md`](../docs/architecture.md). Tier semantics:
> [`tiers.md`](tiers.md). The browser UI (`/`, `/n/*`, `/graph`, `/al-mina`,
> `/setup`) is not part of this contract and uses a different credential.

**Base:** `https://null.<host>/v1` (plus `/mina` at the root, see §5)
**Auth:** `Authorization: Bearer <NULL_TOKEN>` on every route except `GET /v1/health`. One static token, rotated by hand — do not build OAuth for a single user. (The browser UI uses a separate `NULL_UI_TOKEN`; `nullmcp` speaks OAuth, wrapped around one secret.)
**Errors:** `{ "error": "code", "detail": "..." }` with the obvious status codes.
**Wire format:** compact JSON (`json.Marshal`, never indented), `application/json`.

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
| `tier` | enum | **(tiers)** `dakhil` \| `amil` \| `thabit` \| `asil`; exact match. Unknown value → `400` |
| `updated_after` | ISO 8601 | mtime filter |
| `updated_before` | ISO 8601 | **(tiers)** mtime filter; with `tier=dakhil` this is the staleness sweep |
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
      "tier": "thabit",
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

`section` matters more than it looks. It is what lets a caller pull one clause out of a long note instead of the whole thing.

**Response**
```json
{
  "path": "engineering/basim/soul.md",
  "tier": "thabit",
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
| `folder`, `tag`, `tier` | | same as `/notes` |
| `limit` | int | default 20, max 50 |

**Response — snippets, never bodies**
```json
{
  "results": [
    {
      "path": "philosophy/barzakh.md",
      "title": "barzakh",
      "tier": "amil",
      "score": 0.82,
      "matches": [
        { "line": 14, "snippet": "...the interval between states, unresolved..." }
      ]
    }
  ]
}
```

**Scores are weighted by tier (tiers):** the raw score is multiplied by `dakhil` 0.4, `amil` 0.8, `thabit` 1.0, `asil` 1.0. Raw capture ranks below reviewed notes but is never excluded.

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
    { "path": "engineering/basim/character.md", "title": "character", "distance": 1, "tier": "thabit" }
  ],
  "edges": [
    {
      "from": "engineering/basim/soul.md",
      "to": "engineering/basim/character.md",
      "context": "If character.md and this file ever disagree, this file wins.",
      "tier": "thabit"
    }
  ]
}
```

**(tiers)** Every node carries its tier; every edge carries the **lower** of its two endpoints' tiers — an edge is only as trustworthy as its weaker end. Nothing is excluded by tier.

The `context` field — the line the wikilink appeared on — is the whole prize. Embedding search tells you two notes are *near*. This tells you *why they were connected*, in your own words, at the time you connected them. No vector store recovers that.

---

## 5. `GET /mina` **(tiers)**

Al-Mina's queue as JSON, read-only: every note with an outstanding tier proposal, plus `dakhil` notes untouched for `NULL_MINA_STALE_DAYS` (default 14). Oldest first. Acting on an entry (approve / deny / defer) happens only in the browser at `/al-mina`, never through this API.

```json
{ "entries": [
  { "path": "notes/idea.md", "title": "idea", "type": "proposal",
    "tier": "amil", "proposed_tier": "thabit", "reason": "reviewed across 3 sessions",
    "age_days": 4.2, "changed_since_review": false },
  { "path": "notes/old.md", "title": "old", "type": "stale_dakhil", "tier": "dakhil", "age_days": 31 }
] }
```

`type` is `proposal` or `stale_dakhil`. `changed_since_review` is a boolean derived from the note's `tier_history`, not a diff.

---

## Implementation notes

**Index in memory, watch the disk.** Parse the vault into a dict on boot, `inotify`/`fswatch` for changes, re-parse only the touched file. A few thousand notes is a few MB — you do not need SQLite until you do.

**Cache invalidation is mtime.** Nothing cleverer.

**Log every request with path and byte count.** This is the log from step 4 of the plan — where Basim looked, what he got back, how big it was. It is how you learn your schema instead of guessing it.

**Rate-limit nothing. Auth once. Ship it.**

---

## Deliberately absent

- Writes, of any kind, through this API (writes exist elsewhere — see the status note at the top)
- In-browser editing of note bodies
- Embeddings, RAG, chunking
- Users, roles, sharing
- Anything touching dotfiles or dot-directories, `.git/` above all

Each of these is a real thing you will want later. None is a thing you can specify correctly today.
