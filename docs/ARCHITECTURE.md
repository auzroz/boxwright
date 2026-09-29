# Architecture

## Data flow

```
[iOS app]
  a photo from the camera, the photo library, or the clipboard
    -> ONE intake: downscaled to a JPEG in the document directory, then added
       to the pending list (MMKV key pending.v1) and identified in the background
    -> POST /api/v1/identify         (vision model -> {items: [ItemDraft, ...]},
                                      or 422 -> manual entry, which is a READY
                                      capture with no items)
  the user reviews it now, or parks it and reviews it from the inbox later
    -> POST /api/v1/recommend        ({items: [...]} -> {recommendations: [...]},
                                      positional; scored as ONE batch so each
                                      item is charged for the capacity and
                                      categories the ones before it took)
  the user picks a destination per item (a container, an area, or a new container)
    -> POST /api/v1/catalog          ({entries: [{item, entryId, boxId|newContainer}]}
                                      -> one Homebox entity per entry, the one
                                      photo attached to each; per-entry results,
                                      so one failure neither rolls back nor
                                      hides the rest. entryId is stored as
                                      boxwrightKey, so a resend after a lost
                                      response files nothing twice)
  anything that could not be filed joins the queue (MMKV queue.v2) and flushes later

  connect, once (server address + token, entered on the phone, Keychain-stored)
    -> GET  /api/v1/status           version, API generation, identification
                                      on/off, and whether Homebox answers

  setup, once (and whenever the structure changes)
    -> GET  /api/v1/locations        every location, with whether it is selected
    -> PUT  /api/v1/locations        record the selection
    -> POST /api/v1/locations/adopt  one-time migration for an existing setup
  read-only, for the pickers
    -> GET  /api/v1/boxes  /api/v1/categories  /api/v1/entity-types

[Backend]
  one instance per Homebox, keyed on the credentials that reached it.
  box index cache (per instance): the SELECTED locations
  (?isLocation=true&fields=boxwrightPlacement=true) + their direct children,
  PLUS the areas above them (areasAbove, one extra unfiltered list call).
  Refreshed on TTL (BOX_CACHE_TTL). A catalog write PATCHES the cache with a
  per-item delta rather than invalidating it -- a rebuild is ~21s for 80
  locations, and every item in a session used to pay it. A selection write, or
  a delta that cannot be applied, invalidates instead.
  Refresh failure serves stale data with stale=true (offline tolerance).
  With HOMEBOX_WATCH (default on) a WebSocket to the operator's own Homebox
  invalidates and pre-warms on edits made anywhere else -- see "Box index
  freshness".

[Homebox v0.26.x]  system of record. Locations nest by parentId to whatever
                   depth the user's own organisation happens to have.
```

## One backend, one Homebox — or several

By default every request resolves to the Homebox in the backend's environment.
That is the self-hosted product, and it stays the default: someone running this
beside their own Homebox should not have to type a token into an app.

With `ALLOW_CLIENT_HOMEBOX=true` a request may carry
`X-Boxwright-Homebox-Url` and `X-Boxwright-Homebox-Token` instead. This exists
because a signed build handed to another person cannot work the other way — their
items would land in the operator's inventory.

Everything cached about a Homebox lives on an **instance** (`internal/api/instance.go`)
keyed by a hash of the URL and token: the box index and its generation counter,
and the in-flight claim table the idempotency check uses. Nothing about a
Homebox is reachable from credentials that did not open it. The map is bounded
and evicts the least recently used, so a backend serving many people does not
grow one box index per person forever.

Two deliberate asymmetries:

- **A token alone is accepted** and reuses the configured URL. It names no host,
  so it adds no request-forwarding surface — it is just a different Homebox
  account on the same instance.
- **A URL alone is refused.** Pairing a caller's URL with the operator's token
  would send the operator's Homebox credential to a host the caller chose.

Restricting *which* Homeboxes are reachable is not attempted here. It is the
operator's decision and belongs in a network policy in front of the service; a
hostname allowlist in this code would read as protection it does not provide.
What is enforced is the scheme, because `url.Parse` accepts `file://` without
complaint.

## Item regions are advisory, and verified before use

`ItemDraft.Region` is where an item sits in the photo, in fractions of the
image, so the app can crop each review card to its own object. Optional
everywhere, and filtered twice before anything is shown:

- **Per box** (`Region.normalized`): anything that does not survive clamping is
  dropped rather than repaired, and so is anything larger than 35% of the frame
  — a crop that big shows the shelf again and cannot tell one card from another.
- **Per set** (`DropFabricatedRegions`): if every coordinate across two or more
  boxes is a multiple of 0.05, the model was writing round numbers rather than
  looking, and the whole set goes.

Both are deliberately biased towards dropping. Being wrong in that direction
costs the user a crop they would not have had anyway; being wrong in the other
direction shows them a confident picture of the wrong object, which is what a
real user noticed within minutes of the feature shipping.

`Region.UnmarshalJSON` cannot fail, for a related reason: before it existed, a
model answering `"region": [0.1,0.2,0.3,0.4]` failed the whole reply and lost
every item in the photo. An optional field must never make the response more
brittle than it was without it.

## Things that do not go in a container

`ItemDraft.Bulky` means an item does not go inside a container at all: a lawn
mower, a bicycle, a floor lamp. It is a separate fact from `SizeBucket` and not
a bucket above XL, because the difference is of KIND rather than degree — the
buckets measure an item against a container ("S fits in a shoebox, XL fills a
large tote"), and a mower is not a large tote-load.

It exists because of a real report: XL was 8 units and an unrecorded capacity
was assumed to be 8, so a photographed lawn mower scored as exactly filling an
empty container and was confidently recommended into one.

A bulky item is placed in an **area** — a location that holds other locations.
`Box.IsArea` is derived from the hierarchy by `areasAbove`, which walks up from
each location the user chose. That is not the shape-inference Phase 1a removed:
that inferred whether a location can hold items, which is a judgement and the
user's to make; this reads which location contains which, which is a fact.

Three rules keep the two kinds apart:

- `item.Bulky != box.IsArea` is a hard exclusion in both directions, checked
  first. A mower is never offered a tote, and a drill is never filed "in the
  Garage".
- Capacity does not apply to an area. A garage does not fill up the way an
  18-gallon tote does, and pretending it holds eight units would let four
  bicycles exhaust a building.
- Areas are ranked outermost first (`AreaDepth`). Asked where a mower goes, the
  user who reported this said "the garage or storage unit 1" — the outer level,
  not the row of totes inside it, which is a grouping rather than floor space.

When a bulky item has no area at all, the answer is `NoPlace`, never
`NewContainer`. Offering to create a box for a lawn mower is the reported bug
wearing a hat.

## Why rules-first placement

The engine (internal/placement) is deterministic: hard exclusions
(bulky-vs-container, capacity, fragile into an explicit crush-risk, heavy into
an explicit not-heavy-safe) then weighted scoring (category affinity share,
capacity headroom, an empty bonus, an access bonus). Benefits:

- Works offline and with AI_PROVIDER=none (privacy and cost story).
- Explainable: every candidate carries reasons shown in the UI.
- Testable: scoring weights are data, covered by table tests.

An LLM Reranker interface exists for a later optional refinement pass over
the rules-based shortlist (3-5 candidate manifests, tiny context). The LLM
is never the primary decision-maker.

## Which locations are placement candidates

**The user chooses. Boxwright does not infer it.**

A location is a candidate because it carries the custom field
`boxwrightPlacement` with the text value `true`, written by four paths: `PUT /api/v1/locations` from the picker,
`POST /api/v1/locations/adopt` for a one-time migration, `createContainer` when
the user accepts a new-container suggestion, and `cmd/bootstrap` for a leaf it
CREATES (never on a re-run, which must not undo turning a location off).
There is no rule about depth, emptiness, item count, or whether the location
holds other locations.

This is the correction of a real bug, not a preference. Boxwright used to infer
containerhood from shape — a location was a candidate if no other location
claimed it as a parent, and if it was not an empty unannotated top-level entry.
Both rules were generalised from one contributor's inventory, and against a
Homebox holding twenty empty boxes at the top level they rejected all twenty:
zero candidates, "no existing box is a good fit" for every item, forever.
People organise differently. Some file into labelled boxes on numbered shelves;
some file into "Attic", "Garage", "Shelf in Pantry". A location that holds other
locations can be a perfectly good place to put a thing. Someone arriving with an
elaborate scheme of their own must be able to point Boxwright at part of it and
have everything else left alone.

Three consequences:

- **Eligibility lives in Homebox, not in our config.** Homebox is the system of
  record (principle 1). The flag is visible and editable in Homebox's own UI,
  survives reinstalling this backend, and belongs to the user's instance rather
  than to ours — which is what the per-user future needs, where the app carries
  its own Homebox credentials.
- **It is stored as TEXT, not boolean.** Homebox's `fields=Name=Value` filter
  matches text fields only (measured: a location holding `capacityUnits=8` is
  not returned by `fields=capacityUnits=8`). Storing it as text makes an index
  rebuild cost one list call plus two requests per *chosen* location, instead of
  one detail request for every location in the instance. The query is an
  optimisation, never the check — each location's own fields are re-read and
  re-tested, so a server that ignores the filter is slow rather than wrong.
- **Names carry no semantic signal, and must not.** Whatever a user calls their
  containers, the engine cannot lean on the name: it has only the category mix
  of the contents and the fields below. That is the reason the product exists —
  a person cannot remember what is in "Bin 4" either.

`POST /api/v1/locations/adopt` is a one-time, explicit migration: it marks every
location already carrying container metadata as a candidate, so someone with
eighty of them does not tick eighty boxes. It never overrides a decision already
recorded, in either direction, and it is safe to run twice. Accepting a
new-container suggestion also opts the created container in, because that is the
user saying to put things there.

## Box metadata model (Homebox custom fields on location entities)

| Field | Type | Use |
|---|---|---|
| `boxwrightPlacement` | text | `true` when the user has selected this location as a placement target. Anything else, including absent, means no. |
| `containerType` | text | The user's own name for the kind of container ("27-gallon", "shoebox"). The list of types is derived from these. |
| `capacityL` | number | How much it holds, whole litres. Absent = unknown, which never excludes. |
| `interiorCm` | text | Inside size, `"70x45x38"`; parsed leniently, unparseable = unknown. |
| `fillPct` | number | How full, 0-100 (an estimate may pass 100). Meaningful only with `fillSource`. |
| `fillSource` | text | `observed` / `lidar` / `estimated`; `""` = unknown. Only observations may exclude. |
| `fillCheckedAt` | text | RFC3339 of the last OBSERVATION; estimates since do not move it. |
| `capacityUnits` | number | **Legacy: neither written nor read.** See "Unknown capacity" below. |
| `access` | text | `easy` / `normal` / `deep` — effort to reach it. |
| `heavySafe` | boolean | Safe for heavy items (floor level, or a sturdy low shelf). |
| `fragileSafe` | boolean | Padded, not crushable, top of stack. |
| `gridX`, `gridY` | number | Spatial map. Deferred. |

`area` is **derived, not stored** — it is the name of the location's parent,
already present in the hierarchy. Storing it would be a second source of truth.

### Unset is not false

`fragileSafe` and `heavySafe` are tri-state in the engine (`*bool`). A hard
exclusion fires only on an explicit `false`, never on an absent field.
Otherwise cold start is self-reinforcing: with no metadata anywhere, every
fragile item is excluded from every container, which produces a new-container
suggestion, which creates a container that also has no metadata, which excludes
the next fragile item. Unset means "unknown" — the box stays a candidate and the
reason string says the safety is unrecorded.

Category rides as a **Homebox tag**, deliberately NOT as a custom field: tags
come back fully expanded on list rows and custom fields do not, so counting
categories through a field would cost one request per item -- 132 for a single
box on the reference instance. Items do get `sizeBucket`, `weightClass` and
`fragile` as custom fields.

### Unknown capacity

Size is a custom field and absent from list rows, so the read path cannot add
up what a container holds -- only count it. `fetchBoxes` records `itemCount`
(quantity included) and reports `capacityUnits` and `usedUnits` as 0.

It used to estimate instead: every item an M (2 units) against an assumed
capacity of 8, and `capacityUnits` read back from Homebox. Those stored values
were Boxwright's own guesses -- a created container got its size bucket's
units -- and the arithmetic read any container holding four things as full.
Capacity is a hard exclusion, so on the first real inventory most established
containers dropped out of every recommendation and the engine kept suggesting
new ones. Now a capacity is only a fact when someone recorded it, and unknown
never excludes, the same rule as the tri-state safety flags. An unknown box
scores `NeutralHeadroom` (as though half full), and `itemCount == 0` is what
makes a box empty.

### Litres, fill and fit

What a container can take is a question of three recorded facts: its capacity
in litres, its inside size, and how full it is -- each set by the user (or a
LiDAR photo) and each allowed to be missing. An item takes its size's volume,
or its bucket's nominal litres. From there:

- Only facts exclude: a capacity smaller than the item, a measured item that
  does not go through a measured opening, a fill someone observed. An estimate
  -- a model's guess at a size, Boxwright's sum of what it filed -- changes a
  score and nothing else.
- Unknown is scored as half full, between a box known to be roomy and one
  known to be nearly full.
- An estimated fill that says the item will not go in costs `OverfullPenalty`:
  the box stays in the list, where the user can overrule a sum of guesses, and
  the new-container suggestion appears beside it.
- Fill is a property of the container, stored on it, not a sum over its items:
  item sizes are absent from list rows, most items were never measured, and
  packing is not additive. What an observation says is the truth until the
  next one.

## Box index freshness

Three mechanisms keep the cached index close to Homebox, and they are not
alternatives — each covers what the one before it cannot see.

**`patchBox` handles our own writes, and it is the accurate one.** A catalog
write knows exactly what changed: one item of a known category, size bucket and
quantity into one known box. The cached counters are updated in place, nothing
is refetched, and the alternative was measured at 4.3s per item in a
cataloguing session. It bows out — and the caller invalidates instead — when the
index has never loaded, when the target box is not in it, or when the change is
a location entering or leaving the chosen set, which no per-item delta can
express.

**The change feed handles everyone else's, and it is advisory.** With
`HOMEBOX_WATCH` on (the default) the backend holds one WebSocket to
`GET /v1/ws/events` on the Homebox in its own environment, so an edit made in
Homebox's web UI reaches the index in seconds rather than after up to a whole
`BOX_CACHE_TTL`. What arrives is 27 bytes that say only "something changed"
(docs/HOMEBOX.md), so the watcher's entire effect on the cache is the two things
the request path already does: `inst.invalidate()`, then a pre-warm through the
same box-index refresh every handler calls. There is deliberately no second
implementation of the commit protocol — the pre-warm differs from a request
only in not recording a read, which would otherwise re-arm the gate below. Four consequences worth knowing:

- **It suppresses its own echoes.** The feed fires for Boxwright's writes as
  well, so `/catalog` and the two location-selection handlers mute their
  instance for the duration of their writes plus a short quiet period
  (`beginWrite`). Without that a ten-item session would answer each of its own
  echoes with a full rebuild and undo exactly what `patchBox` is for.
- **It pre-warms rather than only invalidating**, because invalidating alone
  makes nothing fresher: it moves a ~21s rebuild onto the next request, which in
  the case this exists for — edit on the laptop, pick up the phone — is the
  request somebody is waiting on. The pre-warm is gated on that instance having
  been read within the TTL, so an idle backend makes no unprompted upstream
  calls, and bursts are coalesced and run strictly one at a time.
- **The operator's own Homebox only.** Per-request Homeboxes
  (`ALLOW_CLIENT_HOMEBOX`) stay on TTL plus `patchBox`: a socket per guest
  instance would turn a request-scoped SSRF surface into a persistent outbound
  connection to a caller-named host, held open with no request in flight.
- **Every failure degrades to the behaviour that came before it.** A Homebox
  with no such endpoint, a reverse proxy that ate the `Upgrade` header, a
  refused key, a storage unit with no signal: all of them reconnect with
  backoff and none of them is fatal. `GET /healthz` reports `changeFeed` as
  `connected`, `degraded` or `disabled`.

**`BOX_CACHE_TTL` is the floor, and the only guarantee.** It covers everything
neither of the others sees — including every change made while the socket was
down, because the feed has no resume cursor and announces nothing on reconnect.
That is exactly why the TTL is not lengthened now that there is a push channel:
a silent disconnect would otherwise leave a permanently stale index reporting
`stale=false`. For the same reason the watcher never invalidates on connect or
reconnect; the TTL kept running through the gap, so nothing is staler than it
already was, and a flapping socket would become a rebuild loop.

## Cold start

Someone whose Homebox already describes their house has nothing to do but open
the picker and tick what Boxwright may file into. Someone starting from an empty
Homebox has no locations to tick, so the placement engine has nothing to score.

`cmd/bootstrap` is for the second case and is **optional**. It reads an indented
outline (`storage.example.txt`) and creates the hierarchy, annotating and opting
in the containers the outline declares. It plans by default and writes only with
`-apply`, and it is idempotent by (parent, name) so the outline stays the source
of truth as it grows. It is not on the path of anyone who already has a scheme,
and Boxwright ships no naming scheme of its own — the example outline is a
shape, not a system to adopt.

Three details it gets deliberately right:

- A re-run never re-asserts what the outline is silent about, `boxwrightPlacement`
  included. Turning a location off in the picker is a decision; a later
  `make bootstrap-apply` must not quietly undo it.

- Locations are created with an explicit location `entityTypeId`. Omitting it
  makes Homebox auto-resolve the group's default type, which is a *non*-location
  type, so every "container" would silently be an item that can never hold anything.
  With no location type available it refuses rather than proceeding.
- Unspecified `heavySafe` / `fragileSafe` are left unwritten, not defaulted to
  false. Absent means unknown; false is a permanent exclusion.

## Local model guidance (Ollama)

- `gemma3:4b` — easiest setup, ~3 GB at Q4, decent household object ID.
- Qwen VL variants — best OCR (serial numbers, labels) where runtime
  support exists.
- `moondream` — sub-4 GB hardware.

Local small-VLM accuracy on unlabeled objects is limited by design the UX
treats identification as a draft for human review, never autonomous.

## Mobile

Bare React Native with a committed `app/ios/` Xcode project; no Expo modules
and no Expo services. The app is one `App.tsx` plus the modules in `app/src/`,
with the server settings in `src/ConnectionScreen.tsx`. It has grown past what
a single file wants to be; splitting into screens plus a navigation library is
the obvious next refactor, and the step machine below is the seam to split on.

Native modules, and why each:


| Need | Module | Note |
|---|---|---|
| Queue and read caches | `react-native-mmkv` | Synchronous; see below |
| Server settings | `react-native-keychain` | `WHEN_UNLOCKED_THIS_DEVICE_ONLY` |
| Photo files | `react-native-file-access` | Ships no privacy manifest; the app's declares DiskSpace and FileTimestamp for it |
| Downscale | `@react-native-community/image-editor` | A whole-image crop to 1024px, JPEG q0.6, EXIF (GPS) dropped by the re-encode. Replaced `@bam.tech/react-native-image-resizer`, whose podspec needs pods React Native 0.86 no longer has |
| Camera and library | `react-native-image-picker` | Library via the system picker, so no photo-library permission |
| Paste a copied image | `@react-native-clipboard/clipboard` | Read only when "Paste" is tapped; ships no privacy manifest and needs none (UIPasteboard is not a required-reason API) |



**`App.tsx`** holds an eight-state machine:
`connection | capture -> setup | waiting -> inbox | review -> recommend -> done`.
`connection` comes first on a fresh install, before the Keychain says there is
a server to talk to. Three refs
guard it, and each exists because of a real bug:

- `generation` is bumped whenever a capture ends or a new one starts. Every
  async continuation checks it before touching state, because identify,
  recommend and catalog all take up to 60s and none is cancelled when the user
  leaves. Without it, a settled request wrote its results into a screen that
  had moved on -- once filing a photo under a different photo's item names.
- `picking` is set synchronously when a photo source is tapped. `busy` is React
  state and so is not true until the next render, which is long enough to tap a
  second source.
- `photoQueuedRef` mirrors `photoQueued`, because an `Alert` captures state by
  VALUE when it opens and a flush can hand the photo to the queue while the
  confirmation sits on screen.

**`app/src/offline.ts`** owns both durable queues and the photo garbage
collector. **`app/src/photo.ts`** is the single intake for every photo source,
and **`app/src/crop.ts`** is the only place a region becomes a crop.
**`app/src/categories.ts`** holds the two folds -- one that reshapes for writing, one that loses information for
comparing -- and they must never be merged. **`app/src/api.ts`** wraps fetch
with the retriable/not distinction the queues depend on.

## When the app does work

Identification of a parked capture runs **only while the app is in the
foreground**. React Native has no reliable background execution, and Boxwright
declares no iOS background modes, so a capture parked on the way out of a
storage unit resumes when Boxwright is next opened. `runIdentification` is
triggered on mount, on `AppState` becoming `active`, when a photo is added, and
when the user retries.

Nothing is lost by this: the photo is already durable and the list is on disk.
It only means a queue does not drain in a pocket.

A native build could change it, in increasing order of correctness:
`beginBackgroundTask` (~30s of grace, enough to finish the request in flight),
`BGProcessingTask` (opportunistic, in practice overnight), and a background `URLSession` (the system daemon performs the
transfer while the app is suspended — the mechanism Apple built for exactly
this shape of work, and it would help filing too). None is implemented.

The catalog queue has the same property, with one difference: it also flushes
whenever a foreground catalog write succeeds, since the connection is
demonstrably up at that moment.

## Filing the same capture once

The offline queue retries on a timeout, and a timeout is exactly the case where
the request may have SUCCEEDED and only the response was lost. So every entry
carries an `entryId`, minted once by the app when the entry is built and
persisted with it, stored on the created entity as the text field
`boxwrightKey` inside the `SetFields` call that already happens.

**Not the capture id, and not a position within it.** After a partial failure
the app re-sends the same capture with a DIFFERENT SUBSET of its entries, so
anything positional matches the resent entry against one that already landed
and silently drops it -- worse than the duplicate it was preventing.
`TestSubsetResendIsMatchedByKeyNotPosition` detects that loss.

New containers get a key derived by FNV-1a over label+parent+type, so a resend
finds the container instead of making a second one -- and a container found by
key is never re-annotated, because anything derived from the CURRENT request's
entries would describe less than is inside: a resend carries fewer of them.

Everything fails open, towards a duplicate somebody can delete rather than an
item nobody sees again: a failed lookup, a result set too large to have been
filtered, a failed detail read, and a missing key all mean "create it". A
process-local claim closes the concurrent-send window the query cannot see.

## Who owns a photo file

A JPEG in `Documents/captures/` can be owned by three things:

1. the **pending list** (MMKV `pending.v1`) -- taken, not yet reviewed;
2. the **catalog queue** (MMKV `queue.v2`) -- reviewed, not yet filed;
3. the **capture on screen** -- being reviewed right now.

`loadAll` is the only thing that deletes an unowned one, it runs at startup
only (when nothing is on screen), and it is **skipped entirely if either list
failed to load** -- a reference set built from a failure names nothing, so
collecting against it would delete every photo both lists were holding.
Orphaned photos are cheap; a capture is not.

Two consequences that look like inefficiencies and are not. A capture opened
for review STAYS in the pending list until it is filed or discarded, because
its drafts exist only in React state and removing it on open would make opening
something less safe than leaving it alone. And `discardPending` deletes the
photo only if the catalog queue does not also name it.

Crops are not files at all. A per-item thumbnail is the one capture photo
drawn inside a clipping view (`crop.coverCrop`), so there is nothing to own,
cache or collect, and no decode per item.

Uploads use React Native's own `FormData` `{uri, name, type}` part, which
streams the file from disk without reading it into JS. The type and name
follow the file's extension (`api.imageTypeFor`). Every photo is re-encoded to
JPEG on the way in, so that is almost always image/jpeg; but a photo that could
not be re-encoded is kept under its own extension, and a vision model told a
PNG or HEIC is a JPEG may refuse it.

Downscaling happens once, in `photo.downscale`, and not in the picker: the
clipboard has no picker, and every source should end as the same kind of file.
The library is asked for its "compatible" representation (JPEG, not HEIC), so
even the fallback original is something every vision model reads.

### Which server the app talks to

Chosen on the phone, stored in the Keychain (`src/connection.ts`), and read per
request, so a saved change applies to the next call. Nothing about the server
is built into the bundle: one build serves anyone's server, and no token can be
recovered by unpacking the app.

Everything on the offline queue was decided against one inventory -- its box
ids, its containers, its tags. So a change of **destination** (backend URL,
Homebox URL, or Homebox token) is refused while captures are waiting, and when
it goes through, the box and category caches for the old inventory are
dropped. A change of API token alone is the same destination and is always
allowed. See `offline.switchConnection`.

### Offline

Captures that cannot be filed are persisted (photo + drafts + chosen box)
and flushed when the app returns to the foreground or the user retries,
because storage units have poor signal. See `src/offline.ts`.

The queue lives in MMKV (`src/storage.ts`) rather than in files because its
contract is synchronous: `enqueue` returns only once the capture is stored, and
"Saved on this phone" is shown on the strength of that. Every file API a bare
React Native app can use is asynchronous. MMKV writes through a memory-mapped
file (durable once the call returns, even if the app is killed) and checksums
each write, opened with `recover-on-error` so a damaged store is salvaged
rather than emptied. Photos, at a few hundred kilobytes each, stay files.
