# spec/tiers.md

Replaces the lake/warehouse split entirely. There is no `inbox/`. Curation is a **field**, not a path — a note's directory stays topical for its whole life, and its tier changes independently.

---

## The four tiers

Stored as `tier:` in frontmatter. Ordinal, 1–4. Filenames and folders never encode tier.

| # | Name | Frontmatter | Meaning |
|---|---|---|---|
| 1 | **Al-Dākhil** — الداخل | `dakhil` | Model-created, uncurated. The user may not know it exists. |
| 2 | **Al-ʿĀmil** — العامل | `amil` | User is aware of it. Worked through across at least two sessions. Incomplete or expecting additions. |
| 3 | **Al-Thābit** — الثابت | `thabit` | Curated, proven, persistent. May still take occasional updates. |
| 4 | **Al-Aṣīl** — الأصيل | `asil` | Foundational. Immutable to the model. |

A note with no `tier:` field defaults to `dakhil`. Never guess a higher default.

---

## Enforcement: server-owned, not model-observed

Every rule in this document is **back-end functionality**. The tier field, the permission matrix, R1, R2, the demotion, the denial records — all enforced in Go, inside the handlers. None of it is an instruction to the model.

The distinction is between a rule and a guarantee. A model told not to raise its own tier will eventually raise its own tier. A server that returns an error on that call cannot be talked out of it.

Concretely:

- **`tier`, `proposed_*` and `denied_*` are server-owned frontmatter.** If a model write includes any of them, the server strips them and writes its own values. Silently authoritative, not an error — the model has no legitimate reason to set them and no feedback loop to learn from.
- **R2's demotion happens inside the write handler**, in the same atomic operation as the edit. It is never a follow-up call the model could omit or fail.
- **Al-Mina's approve is a server endpoint with no MCP route.** The only path that raises a tier is unreachable from the model.
- The model's entire tier surface is `tier_get`, `tier_propose`, and lowering-only `tier_set`. No write path to the field exists for it, by construction.

Never implement any of this as prompt instructions, frontmatter conventions the model is trusted to respect, or post-hoc validation that logs a warning. If a rule here can be violated by a model that ignores its instructions, it has been implemented wrongly.

## Wire format

**Compact JSON.** No pretty-printing, no indentation — `json.Marshal`, not `MarshalIndent`. Standard `application/json`, so every client works without a translation layer.

Token-optimized formats (TOON, TRON) were considered and rejected for now: TOON's compression comes with accuracy losses in multi-turn settings, which is exactly this workload. Revisit only if the misses log shows real context pressure, and benchmark on the actual vault before switching.

---

## One door

Everything above is enforced inside the API. None of it fires on a write that reaches the vault another way. So:

**The model has no direct filesystem or git write path.** It commits nothing itself and pushes nothing itself. Every write it makes goes through the API, and the API alone touches the working tree.

Concretely, one of these, not both:

- **Preferred:** the model has no push or commit tool at all. The server commits on write. The user pushes.
- **If a push tool exists, the server owns it.** The model may *request* a push; the endpoint is server-side and runs only against commits the server itself created. It never runs arbitrary git.

A `git push` tool wired directly to the model reopens every rule in this document as a convention rather than a guarantee: it can edit `tier:` in a file and commit it, and path safety, field stripping, the `asil` lock, R1, R2 and the denial check all become decoration. A clerk with his own key to the records room is not supervised, he is trusted — which is a different institution from the one this spec describes.

---

## Permission matrix

Enforced in the write handlers and at the filesystem, per the section above. A tool that structurally cannot perform an action beats a tool instructed not to.

| Tier | create | edit | delete | notes |
|---|---|---|---|---|
| `dakhil` | yes | yes | yes | the model's own working space |
| `amil` | — | yes | no | |
| `thabit` | — | yes → **auto-demotes to `amil`** | no | see below |
| `asil` | — | **no** | no | write-locked at the filesystem |

### Two rules that carry the whole design

**R1 — The model can never raise a tier.** Not by any tool, not at creation, not as a side effect. It may only *propose* (see `tier_propose`). Promotion is a human act. This is the gate; it never bends.

**R2 — Editing a `thabit` note demotes it to `amil`, in the same operation.** Not a warning, not a flag — the write and the demotion are one atomic change. `thabit` means *the user reviewed this text*. The moment the model alters the text, that review is stale, and the note says so without anyone having to remember. This is self-enforcing and requires no discipline from the user.

The model must be told R2 exists. It should be willing to edit a `thabit` note when asked, and should mention the demotion once, plainly, after the fact.

---

## MCP tools

Three, not four. Expiry is not a tool — it is a query (see below).

### `tier_get(path) -> {tier, since, proposed?}`
Current tier, when it last changed, and any outstanding proposal.

### `tier_set(path, tier, reason)`
**Lowering only.** Any call that would raise a tier returns an error naming R1 — it does not silently no-op, because a silent failure teaches the model the call worked. `reason` is required and is written to the note's frontmatter history.

The only automatic call is the R2 demotion, which the write path issues itself.

### `tier_propose(path, tier, reason)`
Writes a proposal into the note's frontmatter (`proposed_tier`, `proposed_reason`, `proposed_at`). Changes nothing else. The user acts on it in **Al-Mina** (below).

Rejects the call if the note already carries a `denied_tier` equal to the proposed one, unless the note has been edited since `denied_at`. Without this, denied proposals return within days and the port becomes a screen the user stops opening.

---

## Al-Mina — الميناء

**The port.** Where Basim's proposals wait and the user approves, denies, or defers them. It is a **view, not a tier** — proposals live in each note's own frontmatter; Al-Mina is the query over them plus a screen to act in bulk. Never `tier: mina`; that would break the ordinal scale.

Manual promotion does not scale note-by-note, so the port exists to make it a single batched pass rather than twenty separate visits.

**What lands in it**
- any note with `proposed_tier` set
- `dakhil` notes older than a threshold, as a staleness sweep (this is the query that replaces `tier_expire`)

**`GET /mina`** returns each entry with: path, title, current tier, proposed tier, Basim's reason, age, and — for an edit-driven proposal — what changed since the last review.

**Actions, all batchable**
- **approve** → `tier_set` upward, the one place R1 is bypassed, because the actor is the user
- **deny** → writes `denied_tier`, `denied_at`, `denied_reason`. Denial is data: it is what stops the port refilling with the same rejections.
- **defer** → clears the proposal, leaves the tier, does not write a denial

**Design constraints**
- Keyboard-driven, one item per keystroke. If approving fifteen notes takes fifteen clicks and a page load each, the port fails at exactly the volume it exists for.
- Show Basim's reason inline. The user is reviewing a judgment, not a filename.
- Port depth in the renderer header, beside the tier counts.

**Note the asymmetry:** 1→2 is automatic — the renderer promotes a `dakhil` note to `amil` the first time the user opens it. So Al-Mina only ever handles 2→3 and 3→4, which is a handful a week rather than the full capture volume.

There is deliberately **no `tier_expire`.** Stale bottom-tier notes surface in Al-Mina via the filter that already exists:

```
GET /notes?tier=dakhil&updated_before=<date>
```

No new concept for something the query layer already does.

---

## Retrieval: weight, do not filter

The earlier lake design excluded uncurated notes from default results. That was wrong — it loses capture. Tiers **weight** instead.

- Search ranking multiplies by tier. Suggested starting weights: `dakhil` 0.4, `amil` 0.8, `thabit` 1.0, `asil` 1.0. Tune from the misses log, not from intuition.
- **Every response entry carries its tier.** Non-negotiable. The model must be able to distinguish settled knowledge from raw capture without parsing paths or guessing.
- `asil` notes are **not ranked at all** — they are always in context, by construction.
- `/graph` traverses every tier. Nothing is excluded. Each node carries its tier; each edge carries the **lower** of its two endpoints' tiers, so an edge is only as trustworthy as its weaker end.

---

## Epistemic semantics live in the vault, not in code

Do not hardcode what the tiers mean into the retrieval layer or the system prompt. Create an `asil` note — `epistemics.md` — that defines how a model should treat each tier. It is always in context, and it makes the vault self-describing: any model pointed at Null learns to read Null from Null.

The tier is a claim about **provenance and review status**, not about truth. A `dakhil` note may be perfectly correct; it simply has not been checked. Phrase the semantics accordingly:

- `dakhil` — *I recorded this; you have not seen it.* Cite as the model's own record, flagged. Never as the user's settled position.
- `amil` — *You have engaged with this; it is in motion.* Reliable, but expect change. Do not cite as final.
- `thabit` — *You reviewed this.* Treat as the user's established position. If the model contradicts it, the model is probably wrong.
- `asil` — *Foundational.* Never argue against it, never route around it.

**Conflict rule, stated explicitly in `epistemics.md`:** when a lower-tier note contradicts a higher-tier one, the model **surfaces the contradiction** to the user. It does not silently prefer the higher tier. Silent preference lets a stale `thabit` note quietly override fresh capture, and the user never learns the `thabit` needs updating.

---

## Renderer

- Tier drives node colour in the graph view. `dakhil` nodes draw **dashed edges** to their neighbours.
- **Tier counts in the header**, always visible: how many notes at each tier. Colour discriminates at fifty nodes and fails at eight hundred; a number scales. This is the accumulation check that replaces `tier_expire`.
- **Al-Mina** is a renderer view: the proposal queue, batch-actionable, keyboard-driven. It is the only surface where a tier can go up.
- The renderer marks a `dakhil` note `amil` the first time **the user** opens it. That read is exactly the evidence tier 2 claims ("user is aware of it"), and the model cannot forge it: the renderer serves the user, the API serves the model. Count only user reads, never model reads, or a note the model keeps re-reading promotes itself on a loop it created.

---

## Scope changes this causes

Two earlier decisions are reversed. Both are intentional:

1. **Graph visualization moves from "deliberately absent" into scope** (M7). It is the primary interface for tier awareness. This is the first and only JS in the build — keep it to one force-directed library, no framework.
2. **`/graph` no longer excludes anything.** The old rule ("never traverses into the lake") is void. Lake notes are marked, not hidden.

`inbox/`, the lake, the promotion report, and the `curated` boolean are all obsolete. Remove them rather than layering tiers on top.

---

## Build order

Slot before the renderer:

- **M6a** — `tier` field parsed, defaulted, indexed. `?tier=` filter on `/notes` and `/search`. Tier present in every response entry.
- **M6b** — write path with the permission matrix. R1 and R2 enforced and tested. `asil` write-locked at the filesystem.
- **M6c** — `tier_get`, `tier_set`, `tier_propose` exposed over MCP. `GET /mina` returning the proposal queue. Denial fields written and honoured by `tier_propose`.
- **M7** — renderer, graph view, tier colours, dashed `dakhil` edges, header counts, and the Al-Mina review screen.

**Tests that must exist:**
- every raise attempt via `tier_set` fails, at every tier boundary
- editing a `thabit` note leaves it `amil`, atomically — no window where it is edited and still `thabit`
- any write to an `asil` note fails, including via path traversal
- a note with no `tier:` field indexes as `dakhil`
- tier appears in every `/notes`, `/search`, and `/graph` response entry
- `tier_propose` refuses a proposal matching an unexpired denial, and allows it once the note has been edited since `denied_at`
- approving from Al-Mina is the only code path that raises a tier
- a model write containing `tier`, `proposed_*` or `denied_*` has those fields stripped; the server's values survive
- responses are compact JSON — assert no indentation in the marshalled output
- no MCP tool exposes git commit, git push, or raw filesystem write; grep the tool registry and assert
