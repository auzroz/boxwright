# Homebox integration notes

Target: **sysadminsmedia/homebox v0.26.x**. The v0.26 "entity merge" unified
items and locations into `/api/v1/entities`; the old `/v1/items` and
`/v1/locations` routes are removed. Do not code against pre-0.26 docs or the
dormant hay-kot original.

## Verified against v0.26.2 (`e01dd73`) on 2026-09-08

Every row below was confirmed against a live instance, not inferred from docs.
Fixtures live in `backend/internal/homebox/testdata/`.

### Confirmed as originally assumed

| Fact | Evidence |
|---|---|
| `tagIds` is the create-body field, not `labelIds` | `repo.EntityCreate` |
| Auth is `Authorization: Bearer <token>` for `hb_…` API keys | 200 on every call below |
| `?isLocation=true` is real, though undocumented in swagger | 9 vs 132 results |
| **Omitting `isLocation` means items-only, not "all"** | no-param total (132) == `isLocation=false` total (132) |
| `parentIds` genuinely binds — it is not silently ignored | real id → 132, `0000…0000` → 0 |
| `parentIds=X` and `parentIds[]=X` behave identically | both → 132 |
| Entity type auto-resolves on create when omitted… | `POST` without `entityTypeId` → 201, type `Item` |
| …**to a non-location type**, so creating a location *requires* an explicit location `entityTypeId` | created box came back `isLocation:true` only when passed `d570f26a…` |

### Corrected — the original client was wrong

| Fact | Reality |
|---|---|
| **Custom-field values are typed** | `type` ∈ `text\|number\|boolean\|time`, read from `textValue` / `numberValue` / `booleanValue`. Writing `capacityUnits=8` (number) and `fragileSafe=true` (boolean) and reading them back through a `textValue`-only struct — which the first version of this client had — yielded `0` and `false`. Confirmed end to end: `GET /api/v1/boxes` reported `capacityUnits:0, fragileSafe:false` for a box whose real values were `8` and `true`. |
| **Custom fields are write-only via `PUT`** | `POST /v1/entities` has no `fields`. `PUT /v1/entities/{id}` (full replace, `name` required) is the only write path. GET-then-merge-then-PUT verified working for all three value types. |
| **`parentId` is write-only** | `EntityCreate` accepts it, but `EntityOut`/`EntitySummary` return a nested `parent` **object** and no top-level `parentId`. The scaffold's `Entity.ParentID` is always empty on read. |
| **Attachment multipart requires `name`** | Fields are `file` (required), `name` (required, filename with extension), `type` (optional — the server infers `photo` from an image mime type), `primary` (bool). The first version omitted `name` and hardcoded `type=photo`; both are fixed. |
| **`fields` never appears on list rows** | 0/9 locations, 0/132 items. Only the detail `GET` carries it, so box metadata costs one request per location. |

### Custom-field filtering: `fields=Name=Value` (measured 2026-09-09)

The box index depends on this, so it was measured rather than read.

| Behaviour | Evidence (101 locations on the live instance) |
|---|---|
| A `Name=Value` pair really filters | `fields=access=easy` → 20 of 101 |
| **TEXT fields only** | `fields=capacityUnits=8` → **0**, on an instance where `Amarillo` really holds `capacityUnits: 8` as a *number* field. Numbers and booleans cannot be queried at all. |
| Exact and case-sensitive on both halves | a value that exists with different case matches nothing |
| Repeated `fields` parameters are OR'd | several values cost one query, not one each |
| **`fields=Name` with no `=` is SILENTLY IGNORED** | the response is the entire inventory, with no error |
| The filtered query is not slower | 0.065 s filtered vs 0.44 s for the unfiltered list |

That last row is the trap, and it is why `ListEntitiesByField` takes the name
and the values separately, joins them itself, and rejects an `=` in either half.
A caller that formats the pair can produce the ignored shape from an empty
variable, and the failure is silent in the worst direction: an eligibility check
reads it as "everything is a candidate", and a dedupe check reads it as "already
there, skip the create". The unsafe shape is made unrepresentable instead of
guarded at each call site.

Both call sites guard as well, so the query is an optimisation and never the
decision. `fetchBoxes` re-tests each location's own fields with
`eligibleForPlacement`, so a server that ignored the filter is slow rather than
wrong. The dedupe check bails when the result set is larger than the number of
keys could explain (`maxDedupeLookups`), and reads each candidate's detail to
attribute it to a key -- filtered rows omit `fields`, so with three keys OR'd
and two rows back, nothing in the response says which row belongs to which.

It is also why `boxwrightPlacement` is stored as **text** `"true"` rather than a
boolean. A boolean would be the natural modelling choice and would render as a
checkbox in Homebox's UI, but it could not be queried, and the box index would
cost one detail request per location in the instance instead of one per location
the user actually chose.

### The custom fields Boxwright writes

All go through `SetFields`, which is GET-then-merge-then-PUT. The PUT is a FULL
REPLACE and rejects some keys the GET returns, so the client strips or
translates `createdAt`, `updatedAt`, `attachments`, `children`, `imageId`,
`thumbnailId`, `totalPrice`, `itemCount`, `parent`, `entityType` and `tags`,
re-sending `parentId`, `entityTypeId` and `tagIds` as flat ids. Everything else
round-trips as raw JSON, so a column we do not model survives — a typed struct
was measured destroying seven of them in one call.

| Field | Type | On | Meaning |
|---|---|---|---|
| `boxwrightPlacement` | text | location | `"true"` = selected for placement. TEXT because only text fields are queryable |
| `boxwrightKey` | text | item, container | idempotency key, so a resend files nothing twice |
| `containerType` | text | location | the user's own name for the kind of container |
| `capacityL` | number | location | whole litres; absent = unknown |
| `interiorCm` | text | location | `"70x45x38"` |
| `fillPct` | number | location | 0-100, meaningful only with `fillSource` |
| `fillSource`, `fillCheckedAt` | text | location | `observed`/`lidar`/`estimated` or `""`; RFC3339 of the last observation |
| `capacityUnits` | number | location | **legacy**: no longer written or read; its values were Boxwright's own bucket guesses |
| `access` | text | location | `easy` / `normal` / `deep` |
| `heavySafe`, `fragileSafe` | boolean | location | tri-state: ABSENT means unknown and never excludes |
| `gridX`, `gridY` | number | location | spatial map; stored, currently unused |
| `sizeBucket`, `weightClass` | text | item | |
| `fragile` | boolean | item | |

Category is **not** here: it rides as a Homebox tag, because tags come back
fully expanded on list rows and custom fields do not.

### Also relied on

- `DELETE /v1/entities/{id}` → 204. Used only to clean up test canaries;
  Boxwright never deletes a user's inventory.
- `POST /v1/tags` creates one; `GET /v1/tags` is a bare array. `ResolveTag`
  matches case-insensitively through a caller-supplied fold, so a category key
  round-trips to the user's own tag instead of creating a near-duplicate.
- Attachments: `POST /v1/entities/{id}/attachments`, multipart, `primary` set.
  The same bytes go up once per entity — attachments belong to one entity and
  there is no way to point several at one blob.
- Paging works: `page` and `pageSize` are honoured, and the client stops on a
  short page as well as on the reported total, so a server that ignores paging
  terminates instead of looping.
- `parentId` on create (measured 2026-09-28, v0.26.2 binary): an id that does
  not exist is `404 {"error":"Not Found"}` and nothing is created -- which is
  how a catalog entry for a box deleted in Homebox fails. But the **nil UUID**
  (`00000000-0000-0000-0000-000000000000`) is read as *no parent*: `201`, and
  the item lands at the top level. The catalog handler refuses a nil `boxId`
  for that reason (`isNilUUID`).
- Registration and keys, as the e2e harness uses them: `POST
  /v1/users/register` `{name,email,password}` → 204; `POST /v1/users/login`
  `{username,password,stayLoggedIn}` → `{"token":"Bearer …"}`; `POST
  /v1/users/self/api-keys` `{name}` with that session → 201 with
  `{"token":"hb_…"}`. A fresh group has only a `Location` entity type; the
  first item created without `entityTypeId` creates `Item` on the spot.

### Corrected — claims *we* made that did not reproduce

Recorded because acting on them would have been wasted work:

- **There is no default page size.** Omitting `pageSize` returns *every* row with
  `pageSize:-1` (132/132). The box index was never being silently truncated.
  Explicit paging is still worth sending as a guard against large inventories
  and future server changes, but it is not a live bug.
- **Empty collections are `"items": []`, never `null`.** The dual-shape decoding
  defence in the client is unnecessary against this server.

### Shape reference

- List responses are **always** `{items, page, pageSize, total, totalPrice}`.
  Bare arrays: `/v1/entity-types`, `/v1/tags`, `/v1/entities/tree`, `/v1/entities/{id}/path`,
  `/v1/entities/fields`.
- `parent` is present on a list row when the entity has one, omitted at the root.
- `itemCount` appears only on locations, and only when non-zero.
- `GET /v1/entities/tree?withItems=` returns the whole hierarchy in one call.
- `GET /v1/status` is unauthenticated and reports `build.version` — use it as the
  version gate before trusting anything here.
- Change feed: `GET /v1/ws/events`, measured in its own section below.
- API keys require the server env `HBOX_AUTH_API_KEY_PEPPER`
  (`Config.Auth.APIKeyPepper`), which must stay stable across restarts.

### The change feed: `GET /v1/ws/events` (measured 2026-09-10)

A standard RFC 6455 upgrade beneath the same base as everything else, so the
full path is `/api/v1/ws/events`. Measured against the live v0.26.2, part of it
with two sockets open at once. The client is
`backend/internal/homebox/events.go` over the framing in `wsframe.go`.

| Behaviour | Evidence |
|---|---|
| Bearer auth, the same credential as every REST call | `Authorization: Bearer hb_…` → 101 |
| No `Authorization` header → **401** | `{"error":"authorization header or query is required"}` |
| A rotated or otherwise invalid key → **401** | `{"error":"valid authorization token is required"}` |
| An application keepalive every 10s | TEXT `{"event":"ping"}`, 16 bytes. It needs no reply |
| **A CONTROL ping (opcode 0x9, empty) every 54s, and its masked pong is MANDATORY** | unanswered: ping at t=54.0, peer closed at t=60.1. Answered: the session survived three pings to t=190 |
| The mutation event carries nothing at all | TEXT `{"event":"entity.mutation"}`, 27 bytes — no id, no entity type, no operation, no timestamp, no group |
| It fires for **items** as well as locations | a plain `POST /v1/entities` produced one; the `DELETE` produced a second |
| Four other events share the socket | `tags.mutation`, `user.mutation`, `export.mutation`, `import.mutation` |
| **The bus is per-group** | two live sockets, both directions: a change in a second account's group produced `entity.mutation` on that account's socket ONLY, and a change in the operator's group produced one on the operator's socket ONLY |

The 54-second control ping is the row that costs the most to rediscover. The
10-second `{"event":"ping"}` is *not* what holds the socket open, so "traffic is
flowing" is no reason to skip the pong — and the disconnect that follows an
unanswered one is a clean close, which reads as a peer that went away politely
rather than as a client that misbehaved. It is also why the client reads frames
on a goroutine of its own rather than inside the caller's `Next`: the caller
here can spend ~21 seconds in a box-index rebuild and the pong has about six
seconds of grace. Every client-to-server frame is masked, control frames
included.

The 27 bytes are the other half. Nothing on this feed can be an incremental
update — every event means no more than "go and look" — and since it fires for
our own writes too, an echo cannot be told from somebody else's edit by anything
in the frame. That is why Boxwright suppresses its own by TIME (`beginWrite` in
`backend/internal/api/instance.go`) rather than by correlating ids or counting
expected events, and why the feed can only ever be an accelerator on top of
`BOX_CACHE_TTL`: there is no resume cursor, so nothing that happened while the
socket was down is announced when it comes back.

The token goes in the header and only in the header. Homebox accepts it as a
query parameter as well — which is what the first 401 body above is telling you
— and that form puts the key in a URL, where any request log between here and
Homebox keeps it. What Homebox's own logs do with it was not measured; the
header costs nothing, so there was no reason to find out.

### The web UI theme: `GET /v1/users/self/settings` (2026-09-30)

v0.26 syncs the web UI's preferences to the server, per USER (not per group):
`GET /api/v1/users/self/settings` returns `{"item": {...}}`, a free-form
object the browser writes with `PUT` on the same path. The theme is
`item.theme`, a DaisyUI name (`homebox`, `garden`, `forest`, ...; the list is
`frontend/lib/data/themes.ts`). A user who never changed it has no `theme`
entry at all, and the web UI then shows `homebox`, so that is what Boxwright
reports for an absent value. Both routes sit behind the same user middleware
as every other `/v1` route (`backend/app/api/routes.go`).

Whether an API key (not only a session) can read it is measured by the e2e
job: the session `PUT`s `{"theme":"forest"}` and `/status`, holding only the
API key, must report `homeboxTheme: "forest"`. A read that fails is cached as
"unknown" and never marks Homebox down.

The colours are not in the API. The app maps the name to that theme's
`--primary` in `frontend/assets/css/main.css` (src/theme/homebox.ts), and
darkens it where white text on it would fall short of 4.5:1.

### Backup caveat

`GET /v1/entities/export` returns **CSV of items only**. Locations appear as a
text name in `HB.location`, so the export is *not* a location backup. Anything
that creates or restructures locations needs a different rollback plan.
