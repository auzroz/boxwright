# Boxwright

AI inventory capture and storage placement recommendations for Homebox.
Photograph an item, identify it with a vision model, get a recommendation
for which existing box it belongs in (or a new-container suggestion when
nothing fits), and file it into Homebox as the system of record.

The name is settled: **Boxwright** (a wright makes or arranges; this one arranges
boxes). Chosen 2026-09-08 after `stowage` failed an availability check — that name
collides with an active .NET storage SDK and every common TLD is registered.
`github.com/boxwright`, npm, Docker Hub and boxwright.{app,dev,io,sh} were all free.

## Product principles

1. Homebox is the backend. We store no inventory data of our own beyond a
   read cache. Everything durable lives in Homebox via its API.
2. Rules first, LLM optional. The placement engine must produce a useful
   recommendation with zero AI calls (deterministic scoring). Vision
   identification and LLM re-ranking are enhancements, never requirements.
3. Model-agnostic where it counts. Five providers: `openai`, `ollama`,
   `anthropic`, `claude-code` (dev only), `none`. The Anthropic provider does
   carry a default endpoint and model (chosen by `make identeval`), which is a convenience and not a
   dependency -- `AI_BASE_URL` still overrides it, and no code path assumes a
   vendor. `AI_PROVIDER=none` must keep the app fully usable with manual entry,
   and a 422 from any provider routes to the same manual-entry form.
4. Offline-tolerant. Storage units have bad signal. The app keeps TWO durable
   queues in MMKV -- captures waiting to be identified (`pending.v1`) and
   captures waiting to be filed (`queue.v2`) -- and the backend serves stale cache
   rather than failing. A photo file can be owned by either queue or by the
   capture on screen; `loadAll` is the only place that collects unowned ones.
   The in-app demo (src/demo/) is a separate world: its own MMKV instance
   (`boxwright-demo`) and `demo-captures/` directory, switched by
   `useDemoStore`/`useDemoCaptures`, deleted on leaving. Preferences alone
   are shared (`readDeviceText`). Nothing in the demo touches the network.
5. No telemetry. Ever. This is a self-hosted, privacy-first project (AGPL-3.0).
6. One backend may serve several Homeboxes, but only when told to. Everything
   cached about a Homebox is keyed on the credentials that reached it, so two
   callers can never see each other's containers. `ALLOW_CLIENT_HOMEBOX` is off
   by default; a backend that was not told to accept client credentials never
   connects to a URL a client named.
7. The user's organisation is theirs. Boxwright files into the structure they
   already have, wherever they say it may. It ships no naming scheme, imposes
   no hierarchy, and never infers from the shape of an inventory what should
   count as a container. See "Whose setup is it" below.

## Architecture

```
app/        bare React Native (TypeScript), no Expo — capture, review, file
  ios/                  committed Xcode project; fastlane/ uploads to TestFlight
  src/connection.ts     server address + tokens, set in the app, Keychain-stored
  src/storage.ts        MMKV: the offline queue + read caches, synchronous
  src/screens/          one file per screen; App.tsx owns the capture and routes
  src/components/       item card, destination list, fill check, depth camera
  src/ui/ src/theme/    Direction A ("Kraft") primitives and tokens; the accent
                        is the only colour that can change (ThemeProvider)
  src/draft.ts          the capture under review, pure, shared by the screens
  src/prefs.ts          per-phone preferences (units, colours), one MMKV key
  src/demo/             the in-app demo: sample inventory answering every API
                        call (backend.ts), isolation (session.ts); for App Review
  src/setup.ts          first-run setup steps; an existing user is never sent
                        through it (saved connection + no record = done)
  assets/fonts/         Fraunces (SIL OFL) for titles, listed in UIAppFonts
  scripts/              check-ios-config.sh: asserts Info.plist + privacy manifest
backend/    Go 1.23+, stdlib only (no external modules yet)
  cmd/server/           entrypoint
  internal/config/      env-based config
  internal/homebox/     Homebox v0.26.x API client (/v1/entities) and its
                        change feed (/v1/ws/events, RFC 6455 by hand)
  internal/ai/          vision providers: openai, ollama, anthropic,
                        claude-code (dev only), none
  internal/placement/   rules-based placement engine + tests
  cmd/bootstrap/        OPTIONAL outline -> Homebox locations
  cmd/corpus/           golden-corpus harness for grading the engine
  internal/api/         HTTP handlers, per-Homebox instances + box index cache,
                        location selection, idempotent catalog writes, the
                        change-feed watcher (watch.go)
  internal/bootstrap/   OPTIONAL outline -> Homebox locations (cmd/bootstrap)
deploy/     kubernetes/ example (one replica, Recreate -- see the file)
docs/       ARCHITECTURE.md (data flow), HOMEBOX.md (verified API behaviour),
            RELEASE.md (the v0.1.0 plan and its gates), PRIVACY.md
```

Request flow. A photo comes from the camera, the photo library or the
clipboard, is downscaled to one JPEG in the document directory, and joins the
pending list. Then:

    POST /api/v1/identify   one ItemDraft per distinct object (an ARRAY)
    POST /api/v1/recommend  every draft from one photo scored TOGETHER, so the
                            second competes for what the first just consumed
    POST /api/v1/catalog    one Homebox entity per entry, the shared photo
                            attached to each, per-entry results

Plus `GET/PUT /api/v1/locations` and `POST /api/v1/locations/adopt` for the
picker, `PUT /api/v1/containers` to record a container's type, size, access,
safety or fill on several at once, and `GET /api/v1/boxes` (which also returns
the user's `containerTypes` and the `sizeLitres` map), `/categories`,
`/entity-types`.

Every one of identify, recommend and catalog is positional and batched:
`results[i]` belongs to `entries[i]`. Read every result -- a 201 means at least
one entry landed, never all of them.

## Commands

```
make test          # gofmt -l (fails on any output) + go vet + go test ./...
make app-test      # the app's jest suite; NOT a dependency of `make test`,
                   # which must run in a checkout with no node_modules
make run           # build then run the backend (needs env, see .env.example)
                   # NOT `go run`: it recompiles each start and keeps the
                   # toolchain resident, which on this 16 GB machine already in
                   # swap gets the server killed by the memory watchdog.
cd app && npm ci && bundle install && (cd ios && bundle exec pod install)
cd app && npm start / npm run ios              # Metro + Simulator (macOS)
cd app && npm run typecheck && npm test && npm run check:ios
cd app/ios && bundle exec fastlane beta        # archive + TestFlight (macOS)

# There is NO container runtime on this machine, so `make docker` and
# `docker compose up` cannot be run here. CI is the only Docker and multi-arch
# oracle -- treat a red docker or release job as a real break. The same goes
# for .github/workflows/ios.yml and the native compile.
```

## Conventions

- Go: stdlib only until there is a concrete reason to add a dependency.
  Table-driven tests. Errors wrapped with %w and context. No global state
  except main wiring.
- TypeScript: strict mode. Types in app/src/types.ts mirror the Go JSON
  contracts exactly; if you change one, change both in the same commit.
- Commits: conventional commits, DCO sign-off required (`git commit -s`).
- Every feature lands with tests for internal/placement and internal/homebox
  request construction. HTTP handlers get httptest coverage.

## Homebox API — critical knowledge

Target: sysadminsmedia/homebox v0.26.x ONLY (the actively maintained fork;
hay-kot original is dormant). The v0.26 "entity merge" unified items and
locations into `/api/v1/entities`; old `/v1/items` and `/v1/locations`
endpoints are REMOVED.

- Locations and items are both entities. `isLocation` lives on the entity
  *type*, not the entity. Hierarchy is `parentId` on write, but a nested
  `parent` object on read -- there is no top-level `parentId` in responses.
- Auth: `Authorization: Bearer <token>` for both session tokens
  (`POST /api/v1/users/login`) and static API keys (`hb_...`, created via
  `POST /api/v1/users/self/api-keys`). API keys require the server env
  `HBOX_AUTH_API_KEY_PEPPER` or Homebox will not start.
- Query children: `GET /api/v1/entities?parentIds=<id>` (repeatable). Locations
  only: `?isLocation=true`. **Omitting `isLocation` returns items only, not
  everything.**
- Create item: `POST /api/v1/entities`. Omitting `entityTypeId` auto-resolves
  the group's default *non-location* type -- so creating a **location** requires
  passing a location type's id explicitly, or you silently get an item.
- Custom fields are typed (`text`/`number`/`boolean`/`time`) and are writable
  **only** via `PUT /api/v1/entities/{id}`, which is a full replace. Read them
  from `textValue`/`numberValue`/`booleanValue`, never `textValue` alone. They
  are also ABSENT from list rows -- only the detail GET carries them, which is
  why the box index costs a request per location.
- `homebox.SetFields` round-trips the entity as RAW JSON on purpose. A typed
  struct silently blanks every column it forgot to model; an earlier one was
  measured destroying purchasePrice, purchaseFrom, serialNumber, notes,
  manufacturer, modelNumber and insured in a single call. Do not "clean it up".
- The custom fields Boxwright writes: `boxwrightPlacement` (text, on
  locations), `boxwrightKey` (text, the idempotency key, on items and created
  containers), `access`, `heavySafe`, `fragileSafe`, `gridX`/`gridY`
  (locations), and `sizeBucket`, `weightClass`, `fragile` (items), plus
  `dimensionsCm`/`dimensionsSource` on an item only when it was MEASURED.
  Category is deliberately a TAG, not a field -- tags come back on list rows
  and fields do not.
- Fill is written once per destination container after each `/catalog`
  (`writeFills`, read-modify-write via `homebox.UpdateFields`, serialised by
  `instance.fillMu`): a `fillAfter` observation is written unless Homebox holds
  a newer one; otherwise the created items' litres are added to a KNOWN fill as
  `estimated`, only if the capture came after the last observation. Deduped
  entries add nothing, so a resend never double-counts. A container with no
  known fill is not even read. A failed fill write is `fillError`, never
  retried: the container then reads roomier than it is, the safe direction.
- What a user records about a container, read by the box index:
  `containerType` (text, their own name), `capacityL` (number, whole litres),
  `interiorCm` (text, "70x45x38", parsed leniently), `fillPct` (number) with
  `fillSource` (text: `observed`/`lidar`/`estimated`; "" = unknown, and it is
  the SOURCE that decides whether a fill is known -- 0 is a real fill and
  Homebox cannot delete a field) and `fillCheckedAt` (text, RFC3339, the last
  observation only). The list of container types is derived from these; none
  is shipped and none is stored anywhere else.
- `capacityUnits` is neither written nor read. The values in the wild are
  Boxwright's own size-bucket guesses, and read back as a recorded capacity
  they excluded established containers from every recommendation.
- The web UI theme is `item.theme` in `GET /api/v1/users/self/settings`
  (per user, free-form; absent means `homebox`). `/status` reports it as
  `homeboxTheme`, cached 10 min per instance; the app maps the NAME to a colour
  (src/theme/homebox.ts) because the API carries no colours.
- Attachments: `POST /api/v1/entities/{id}/attachments` (multipart). `file` and
  `name` are both REQUIRED; `type` is optional and inferred from the mime type.
- The change feed (`GET /v1/ws/events`, Bearer as everywhere else) is
  **per-group**: measured with two live sockets in both directions, a change in
  another account's group never reached ours. So a shared Homebox costs no
  spurious rebuilds. It fires for items as well as locations, ours included.
- `?fields=Name=Value` filters on custom fields, but **only text ones** --
  numbers and booleans match nothing. It is exact and case-sensitive on both
  halves, repeated parameters are OR'd, and **`fields=Name` with no `=` is
  silently ignored and returns the entire inventory**. Never format that pair at
  a call site: use `homebox.ListEntitiesByField`, which joins it and rejects an
  `=` in either half.

The API surface above was verified against a live v0.26.2 instance on
2026-09-08, except the `fields=` filter behaviour, measured on 2026-09-09, and
the change feed, measured on 2026-09-10 -- see docs/HOMEBOX.md for the evidence
and backend/internal/homebox/testdata/ for the captured response shapes. There
are no open Homebox verifications; do not re-litigate them from docs alone.

## Placement engine contract (internal/placement)

Input: an `ItemDraft` (name, category, `SizeBucket` S/M/L/XL, `Fragile`,
`Bulky`, `WeightClass`, `Notes`, `Confidence`, `Quantity`, optional `*Region`,
optional `*DimensionsCm` with `DimensionsSource` lidar/manual/vision) plus the
box index. Each `Box` carries `Categories` (counts by key), `ItemCount`
(counted from Homebox), `ContainerType`, `*CapacityL`, `*InteriorCm`,
`*FillPct`/`FillSource`/`FillCheckedAt`, `Access` (`easy`/`normal`/`deep`),
tri-state `*bool` `HeavySafe`/`FragileSafe`, and `IsArea`/`AreaDepth`.
`CapacityUnits`/`UsedUnits` are derived for old clients and never read.

Sizes are litres. An item takes its dimensions' volume, or its bucket's
`NominalLitres` (S 2, M 8, L 25, XL 60 -- a bucket is the container it FITS,
so a typical member is about a third of it) times quantity. The app gets that
map from `/boxes` rather than keeping a copy.

Output: ranked `Candidates` with human-readable reasons, plus AT MOST ONE of
`NewContainer` (nothing fits, add a container) or `NoPlace` (a bulky item with
no area to stand it in -- never offer to buy a box for a lawn mower). The
new-container fallback is a headline feature; never remove it.

Hard exclusions, in order: `item.Bulky != box.IsArea` (both directions);
then, for containers only, `canHold` -- which excludes on FACTS only: need
beyond a recorded capacity x1.1; a MEASURED item (lidar/manual) that does not
fit a recorded interior, longest side to longest side, +5%; an OBSERVED fill
(observed/lidar) at 100%, or past 110% with this item in it. Then `Fragile`
into an explicit `FragileSafe: false`; `WeightClass == "heavy"` into an
explicit `HeavySafe: false`. **A tri-state nil never excludes** -- unset means
unknown, and treating it as false makes cold start self-reinforcing. **Neither
does an unknown capacity or fill, nor any estimate.** An unrecorded capacity
used to be assumed 8 units with every item charged 2, so any container
holding four things read full and vanished; the first real use found it. A
vision-estimated size and an estimated fill only ever change a score.

Then weighted scoring: category affinity by matching share (the dominant
signal); capacity headroom, `CapacityHeadway x (1 - projected fill)` when
capacity and fill are known (an empty box counts as fill 0), else
`NeutralHeadroom` (scored as half full); `OverfullPenalty`, scaled, when an
ESTIMATED fill says it will not fit -- the box stays listed and the
new-container suggestion appears beside it; an empty bonus, with an empty box
the item fits in never scoring below `MinViableScore`; and an access bonus for
frequently-accessed categories in an `easy` location. Areas are scored on what
they are and ranked outermost first.

A new-container suggestion names one of the user's own container types when
one suits -- the smallest holding twice the item, else the largest holding it
-- and falls back to the size bucket when they have recorded none.

There is NO zone model. `gridX`/`gridY` are stored and unused; there is no
floor-level or front-of-unit reasoning, only `access`. Do not write those into
docs again.

Scoring weights live in one place (`DefaultWeights` in engine.go) and every
change needs a test. One invariant is enforced by test:
`CapacityHeadway + AccessBonus < MinViableScore`, so convenience alone can
never beat the new-container suggestion; `NeutralHeadroom` is held to the same
bound and never exceeds `CapacityHeadway`. And emptiness must not outrank
affinity: a half-full box of the same things beats an empty one
(`TestAffinityStillBeatsEmptinessInLitres`).

## What costs time to rediscover

Measured, not estimated. Do not re-derive these.

| Fact | Value |
|---|---|
| Identify latency, Sonnet 5.5 | p50 2.3s for 1 item, 13s for a dense scene of ~19 (Opus 5: 5.8s, 27s) |
| Identify cost, Sonnet 5.5 | $0.016/photo mean over the eval set. **~58% is INPUT**: the prompt is ~4,700 tokens |
| Model choice | `make identeval` (docs/IDENTIFICATION.md). Sonnet 5.5 found every labelled item; Opus 5/5.5 sometimes answer a crowded photo with ONE item named "placeholder" or "x" |
| Cold box-index rebuild | ~21s for 80 chosen locations (1 list + 2 calls each, sequential) |
| Region grounding | Sonnet 5 correct. Haiku 4.5 and gemma3:4b fail; their boxes are discarded |
| Thinking | Per model family (internal/ai/tuning.go). Declining it was right for Opus/Sonnet 5 (1212 extra output tokens, no better). On Sonnet 5.5 adaptive is BETTER at no extra cost, so `auto` = adaptive there. `thinking: disabled` is a 400 on Opus 5.5 |
| Change-feed control ping | Opcode 0x9 every 54s, and the masked pong is MANDATORY: unanswered, Homebox closes the socket ~6s later. The 10s `{"event":"ping"}` TEXT frame is NOT what holds it open |
| `entity.mutation` payload | 27 bytes, `{"event":"entity.mutation"}` and nothing else -- no id, no type, no operation. Our own writes echo, so self-suppression is TIME-BOXED (`beginWrite`), never correlated |

Environment:

- **The iOS simulator has no camera.** Camera and paste flows are phone-only;
  "Choose an existing photo" (the system picker) works in the simulator. The
  Xcode MCP (`xcrun mcpbridge`) can tap and type through
  `DeviceInteraction*`, on iOS 27+ simulators ONLY (not a physical iPhone).
  The only runtime installed is iOS 27.0 (26.5 was removed 2026-09-28 to fit
  it); use the "Boxwright iPhone 17 Pro (27)" simulator, not a shared one.
  Taps must come from a subagent. The MCP's "device-interaction" skill does
  not exist in Claude Code; the subagent calls DeviceInteractionSynthesize
  directly, in POINTS: tap `t x y`, swipe `t x1 y1 f x2 y2 0.4`. A few
  invalid commands in a row end the session. A session expires during a long first build: build first, then
  start the session and install.
- **Disk is the binding constraint.** A cold build plus a new simulator's
  first boot (it downloads ~1.2 GB of system assets) need 4-5 GB free; the
  disk has hit ENOSPC doing exactly that. Check `df -h /` before either.
- To put a Release build on the maintainer's iPhone without committing a team:
  `xcodebuild ... -configuration Release -destination id=<udid>
  -allowProvisioningUpdates DEVELOPMENT_TEAM=<team> CODE_SIGN_STYLE=Automatic`,
  then `xcrun devicectl device install app`. The build costs ~2.7 GB of
  DerivedData, and the disk often has less than that free: delete it after.
- Metro (`npm start`, bare React Native) listens on `*:8081`, every interface,
  so the Expo-era `--lan` workaround is gone. Run it detached like the backend.
  JS `console.warn`/`error` go to the debugger only, not Metro's log or the
  device log. To read them headless, connect to `webSocketDebuggerUrl` from
  `GET :8081/json/list` WITH `Origin: http://localhost:8081` (without it, 401)
  and send `Runtime.enable`; nothing is replayed, so attach before reproducing.
- CocoaPods needs Homebrew's `ruby@3.3` (the system Ruby is 2.6): prefix
  `PATH=/opt/homebrew/opt/ruby@3.3/bin:$HOME/.gem/ruby/3.3.0/bin:$PATH
  GEM_HOME=$HOME/.gem/ruby/3.3.0` to `bundle exec pod install`. Xcode 27 rejects
  any pod deployment target below iOS 15; the Podfile's post_install raises
  them.
- **This machine is 16 GB with swap nearly full.** Long-lived servers started
  as managed background tasks get killed regardless of size -- a 10 MB binary
  died at 47% free memory. Run them detached:
  `nohup ./boxwright < /dev/null > /tmp/bw-backend.log 2>&1 & disown`.
- The app's test fakes in `app/test/` are hand-written. Two bugs have been
  found IN THE FAKES rather than in the code; check them against the real
  typings in `node_modules` before trusting a green test.

## Whose setup is it

**The product ships no organisation scheme, and infers none.** Which locations
Boxwright may file into is stored per location in Homebox
(`boxwrightPlacement`, text `"true"`) because the user selected them in the app.
There is no rule about depth, emptiness, item count, or having children.

Every rule of that kind that used to exist here was generalised from one
contributor's inventory, and a Homebox holding twenty empty top-level boxes got
zero candidates and "no existing box is a good fit" forever. So:

- Test every product rule against a shape that is not one person's: flat, empty,
  single-location, deeply nested. `TestFlatInventoryOfEmptyTopLevelBoxes` is the
  standing regression test for this whole class of bug.
- `cmd/bootstrap` and `storage.example.txt` are OPTIONAL and are never on the
  path of someone who already has a scheme. The example outline is a shape, not
  a system to adopt; a bootstrap re-run must not undo a choice made in the app.
- Shipped prose, prompts and comments describe *locations that hold things*.
  Do not reintroduce "row", "tote" or a room > row > tote model as though it
  were the product's design.

## Roadmap (ordered)

The release plan, with its gates and the decisions behind them, is
docs/RELEASE.md. Keep this list and that file in step.

1. ~~Verify Go build + tests.~~ Done.
2. ~~Complete the Homebox verifications against a live instance.~~ Done; see
   docs/HOMEBOX.md. There are no open ones.
3. ~~Wire attachment upload into /catalog.~~ Done.
4. ~~App: offline capture queue.~~ Done.
5. ~~Multi-arch release image to GHCR.~~ `.github/workflows/release.yml`.
   Deploy in-cluster next: deploy/kubernetes/boxwright.yaml is the example.
6. ~~App: server address and token set on the phone, not at build time.~~
   `app/src/connection.ts`; the blocker for any iOS build.
7. ~~Box index invalidation via Homebox WebSocket `entity.mutation`.~~ Done;
   `HOMEBOX_WATCH`, on by default. It accelerates OTHER people's edits only --
   TTL plus the in-place patch still cover ours and everything the socket
   missed.
8. **Release v0.1.0** -- backend image, iOS app on internal TestFlight, docs.
   In progress; the remaining gates (in-cluster week, end-to-end pass, first
   fastlane upload, device pass) are in docs/RELEASE.md. Off Expo as of
   2026-09-28: bare React Native, built and signed on the maintainer's Mac. Starts the
   awesome-selfhosted 4-month eligibility clock.
9. External TestFlight / App Store (needs a review demo server).
10. Optional LLM re-ranker behind the Reranker interface in placement.
11. Spatial unit map screen (grid coords from location custom fields).

## What NOT to do

- Do not add telemetry, analytics, or phone-home of any kind. In the app that
  includes crash reporters and over-the-air update services (they contact a
  vendor on every launch); the privacy manifest declares no collection and CI
  checks it.
- Do not reintroduce Expo modules or Expo services (EAS). The app left Expo so
  that nothing but Apple and the user's own server is in its path.
- Do not make an AI call a prerequisite for any core flow.
- Do not store durable inventory state outside Homebox.
- Do not add Go dependencies casually; justify each in the PR description.
- Do not infer from the shape of an inventory what the user should have chosen.
