# Boxwright

Photograph a thing. Know where it goes.

Boxwright is a self-hosted companion for [Homebox](https://github.com/sysadminsmedia/homebox).
You point it at the locations in your Homebox that it may file into, photograph
something, and it drafts the item and recommends where to put it — with reasons.
Homebox stays the system of record; Boxwright keeps only a read cache.

- **Get a photo in three ways**: the camera, a picture already in your library,
  or one you have copied — a product listing, say.
- **One photo, several things.** A shelf shot yields one draft per object, each
  placed independently, and each review card is cropped to its own item when
  the model can locate them.
- **You choose where it may file.** Only locations you have selected are
  placement candidates. Boxwright infers nothing from the shape of your
  inventory and leaves the rest of it alone.
- **A deterministic placement engine** scores those locations on category
  affinity, remaining capacity, access and fragility. It needs no AI call.
- **Things that do not go in a box** — a lawn mower, a bicycle — are placed in
  the location that *holds* your containers: the garage, the storage unit.
- **New-container fallback**: when nothing fits, it suggests the size and
  access of a container to add, and creates it for you.
- **Park a slow one.** Identification takes about five seconds per object in
  the photo, so a cluttered shelf takes a minute. You can walk away and review
  it later.
- **Offline-tolerant.** Captures queue on the device and file when the backend
  is reachable again; the box list is cached so you can still choose a
  destination with no signal.

Privacy stance: fully functional with no cloud calls. Bring your own
OpenAI-compatible endpoint or Anthropic key, point it at local
[Ollama](https://ollama.com), or run with `AI_PROVIDER=none` and enter items
manually — the placement engine is deterministic and never requires an LLM.
No telemetry, ever.

**Status: works end to end, not yet released.** Photograph, identify, place and
file all work against a live Homebox v0.26.2 from an iOS device. What is not
done: a published release -- v0.1.0 is being readied, and
[docs/RELEASE.md](docs/RELEASE.md) lists the gates left -- and any measurement
of the engine's scoring weights against a human's judgement. See
[Known limitations](#known-limitations).

## Quick start (development)

Two processes: the Go backend, and Expo for the app.

```bash
cp .env.example .env      # set HOMEBOX_BASE_URL, HOMEBOX_API_KEY, BOXWRIGHT_API_TOKEN
make run                  # builds, then runs the backend on :8080
```

Get a Homebox API key from Profile → API keys. Homebox must be started with
`HBOX_AUTH_API_KEY_PEPPER` set (32+ chars) or it will not issue one.

Mobile app (bare React Native, iOS; needs a Mac with Xcode):

```bash
cd app
npm ci
bundle install                          # CocoaPods and fastlane, pinned in Gemfile.lock
(cd ios && bundle exec pod install)
npm start                               # Metro, in one terminal
npm run ios                             # Simulator, in another
```

To run on your own iPhone, open `ios/Boxwright.xcworkspace` in Xcode, pick
your team under Signing & Capabilities, and run to the device; a Debug build
loads JavaScript from Metro on your Mac over the local network. The Simulator
has no camera, so use **Choose an existing photo** there.

The app asks for the server address and access token on first launch and
keeps them in the iOS Keychain; change them later under **Server settings**.
**Test connection** checks the whole chain before you rely on it: that the
server answers, accepts the token, can reach Homebox, and whether
identification is on. Nothing about the server is built into the app, so one
build works for anyone's server.

A phone cannot reach `localhost`, so the backend has to bind beyond loopback
(`LISTEN_ADDR=0.0.0.0`, which then requires `BOXWRIGHT_API_TOKEN`). iOS allows
plain `http://` only to an IP address or a `.local` name; anything else, a
Tailscale MagicDNS name included, needs `https://`. `tailscale serve` or the
Tailscale Kubernetes ingress (below) is the shortest way to a certificate.

**First run:** after the server, the app opens on "Choose locations". Nothing
can be placed until you tick at least one — Boxwright will not guess which of
your locations are places you put things. If you are upgrading and already
have containers annotated with `capacityUnits` or `access`, "Bring over
locations I already set up" adopts them in one step.

`docker compose up` also brings up a local Homebox plus the backend, but note
there is no container runtime on the primary dev machine, so that path is
exercised only in CI.

### iOS builds and TestFlight

No build service and no vendor account beyond Apple's: the archive is built
and signed on your Mac, and the app never contacts anything but the server
you point it at (no over-the-air updates, no crash reporter, no telemetry).

```bash
cd app/ios
export ASC_KEY_ID=... ASC_ISSUER_ID=... ASC_KEY_PATH=~/.appstoreconnect/AuthKey_....p8 APPLE_TEAM_ID=...
# (or put the same four in app/ios/fastlane/.env, which is gitignored)
bundle exec fastlane beta               # archive, sign, upload to TestFlight
```

The lane asks TestFlight for the last build number and uses the next one, and
signs with whichever Apple account is signed in to Xcode. See the comments in
`ios/fastlane/Fastfile` for the App Store Connect API key it uploads with.

CI checks the app two ways. `npm run check:ios` (Linux, every PR) fails if the
committed `Info.plist` asks for a permission the app does not use, loosens App
Transport Security, or the privacy manifest declares any data collection or
misses a required-reason API a dependency uses. `.github/workflows/ios.yml`
(macOS) runs `pod install` and a full unsigned Release build for the
Simulator, which compiles every native module and bundles the JavaScript the
same way an archive does.

## Configuration

| Env | Meaning | Default |
|---|---|---|
| `PORT` | backend listen port | `8080` |
| `HOMEBOX_BASE_URL` | Homebox API base, including `/api` | `http://homebox:7745/api` |
| `HOMEBOX_API_KEY` | static API key (`hb_...`) or session bearer token | required |
| `LISTEN_ADDR` | bind address; anything but loopback requires a token | `127.0.0.1` |
| `BOXWRIGHT_API_TOKEN` | shared secret required on every `/api` request | — |
| `ALLOW_CLIENT_HOMEBOX` | accept a Homebox URL and token per request, see below | `false` |
| `AI_PROVIDER` | `openai`, `ollama`, `anthropic`, `claude-code`, or `none` | `none` |
| `AI_BASE_URL` | e.g. `https://api.openai.com/v1` or `http://ollama:11434` | — (Anthropic: `https://api.anthropic.com`) |
| `AI_API_KEY` | key for OpenAI-compatible and Anthropic providers | — |
| `AI_MODEL` | e.g. `gpt-5-mini`, `gemma3:4b`, `qwen3-vl:8b` | — (Anthropic: `claude-opus-5`) |
| `CLAUDE_CLI_PATH` | `claude-code` only: path to the CLI | `claude` (from `PATH`) |
| `CLAUDE_CODE_PERMISSION_MODE` | `claude-code` only: escape hatch, see below | `default` |
| `BOX_CACHE_TTL` | box index refresh interval. A cold rebuild is 1 list call plus 2 per chosen location — ~21s for 80 of them against a LAN Homebox, so do not set this low | `5m` |
| `HOMEBOX_WATCH` | subscribe to Homebox's change feed, so an edit made elsewhere is noticed in seconds instead of after a whole `BOX_CACHE_TTL`, see below | `true` |

### Noticing edits made in Homebox itself

Boxwright caches the containers it scores against, and a write of its own
updates that cache in place. An edit made anywhere else — the Homebox web UI on
a laptop, another client — is invisible to it until the cache expires, which is
the case `HOMEBOX_WATCH` exists for: the backend holds one WebSocket to
Homebox's change feed and starts refreshing the index the moment Homebox says
something changed, rather than on the next expiry.

It watches the Homebox in this backend's own environment and nothing else;
per-request Homeboxes (`ALLOW_CLIENT_HOMEBOX`, below) are never watched.
Everything about it degrades quietly — a Homebox serving no such endpoint, a
reverse proxy that drops the `Upgrade` header, a revoked key, no signal at all —
and `GET /healthz` reports `changeFeed` as `connected`, `degraded` or
`disabled`. `BOX_CACHE_TTL` remains the only guarantee either way, so turning
this off costs nothing but how long an edit takes to notice.

### One backend, one Homebox — or several

By default Boxwright talks to the one Homebox in its environment. That is the
self-hosted product: you run it beside your own Homebox and never type a token
into anything.

`ALLOW_CLIENT_HOMEBOX=true` lets a request bring its own instead, in
`X-Boxwright-Homebox-Url` and `X-Boxwright-Homebox-Token`. Everything cached
about a Homebox — the box index, the tag vocabulary, in-flight write keys — is
scoped to the credentials that reached it, so two people using one backend never
see each other's containers. The token alone is enough and reuses the configured
URL; a URL without a token is refused rather than paired with the operator's own
credential.

**Turn it on deliberately.** With it on, an authenticated caller decides which
host this server connects to, so `BOXWRIGHT_API_TOKEN` stops being only an
access control and becomes the boundary of that too. The server refuses to start
with client credentials enabled, a non-loopback bind, and no token. If you need
to restrict which Homeboxes are reachable, do it with a network policy in front
of this service rather than expecting the app to enforce it.

### Per-item crops need a model that can point

When a photo holds several things, each review card is illustrated with the
photo cropped to its own item — but only if the model can actually locate
objects, which is a capability, not a setting.

Measured on one cluttered desk photo:

| Model | Coordinates on a 0.05 grid | Box size | Crops |
|---|---|---|---|
| Sonnet 5 | 7 of 28 | 0.4%–7% of the frame | ✅ every one the right object |
| Haiku 4.5 | **12 of 12** | 7%–16% | ❌ dropped — one crop was the noticeboard labelled "ASUS laptop" |
| `gemma3:4b` | **28 of 28** | 4%–56% | ❌ dropped — every box wrong; a 42% box labelled "laptop" |

Opus 5 is untested for grounding; it is not measured here and should not be
assumed to behave like Sonnet.

Coordinates landing on a round twentieth every single time is not measurement,
it is a model estimating. Boxwright treats that as the tell, discards the whole
set and logs why, so a model that cannot ground degrades to showing the whole
photo — which is what it showed before this feature existed. A wrong crop is
worse than none: it is a confident picture of one object beside the name of
another.

Note where Haiku lands. It **identifies** the items as well as anything —
"ASUS laptop", "oscillating fan" — and its boxes are roughly in the right part
of the photo, but coarse enough that the laptop's crop is mostly the
noticeboard above it. Naming and pointing are separate capabilities, and only
the larger models have both.

So crops are a bonus on a capable model, not a reason to choose one.
Identification is unaffected either way.

### Vision providers

| `AI_PROVIDER` | Endpoint | Requires |
|---|---|---|
| `none` | — | nothing; every item is entered by hand |
| `ollama` | `POST {AI_BASE_URL}/api/chat` | a local Ollama with a vision model pulled |
| `openai` | `POST {AI_BASE_URL}/chat/completions` | any OpenAI-compatible gateway |
| `anthropic` | `POST {AI_BASE_URL}/v1/messages` | an Anthropic API key |
| `claude-code` | the `claude` CLI on this machine | development only — see below |

The Anthropic provider is ~390 lines of `net/http` against the Messages API,
not the official SDK: that SDK pulls in OpenTelemetry, gRPC, protobuf and the
AWS SDK to send a single POST, which is a dependency surface a no-telemetry
project should not carry. It asks for structured output so the reply is the
item draft and nothing else, and treats a refusal as "identify this one by
hand" rather than an error.

Both `AI_BASE_URL` and `AI_MODEL` default for it (`https://api.anthropic.com`,
`claude-opus-5`), so `AI_PROVIDER=anthropic` plus `AI_API_KEY` is a complete
configuration. Set `AI_BASE_URL` to route through a proxy or gateway instead.

Cost per million tokens (input/output); one photo is a few thousand input
tokens and the reply is a couple hundred, so identification is fractions of a
cent per item:

| Model | Input / output | When |
|---|---|---|
| `claude-opus-5` | $5 / $25 | default; best at reading faded labels and model numbers |
| `claude-sonnet-5` | $2 / $10 | bulk cataloging |
| `claude-haiku-4-5` | $1 / $5 | cheapest; fine for obvious household items |

Use the plain model id — appending a date suffix pins a snapshot that retires.

#### `claude-code`: for evaluation on your own machine, not for deployment

`AI_PROVIDER=claude-code` shells out to the [Claude Code](https://claude.com/claude-code)
CLI instead of calling an API. It is here for one job: judging identification
quality and building a golden corpus on a subscription you already have, before
opening a metered API account. Three reasons it stays a development tool:

**It costs more per photo than the API it stands in for.** Every invocation
re-sends the Claude Code system prompt and tool definitions — roughly 37k
cache-read plus 12k cache-creation tokens of harness that has nothing to do with
your photo. Measured on the same real photo:

| Model | via `claude-code` | via `anthropic` | |
|---|---|---|---|
| Opus 5 | $0.1802, 8.1 s | ~$0.011 | **16x** |
| Haiku | $0.0317, 8.5 s | ~$0.002 | **16x** |

**It cannot run where Boxwright is deployed.** The CLI must be installed and
interactively signed in on the machine running the backend. The Docker image has
no CLI and no login, and a headless server only gets one if someone sits down at
it.

**Claude Code is a developer tool, not an inference API.** Pointing an
application's inference at it is outside what it is for. Fine on your own laptop
while you are grading answers; `AI_PROVIDER=anthropic` is the answer for anything
real. The backend warns about all of this at startup when you select it.

```bash
AI_PROVIDER=claude-code       # AI_BASE_URL and AI_API_KEY are ignored
AI_MODEL=claude-haiku-4-5     # optional; empty defers to the CLI's own model
```

It invokes `claude -p <prompt> --output-format json --allowedTools Read
--permission-mode default`, staging the photo in a `0700` directory as a `0600`
file and deleting it on every exit path. `--permission-mode default` rather than
`acceptEdits`: the only filesystem access this needs is reading back the one
image it just wrote, so pre-approving edits would hand an unattended run write
authority it has no use for; the `Read` allowlist covers the read, and a
non-interactive run has nobody to approve anything else. If a CLI version denies
the read anyway, the error says so by name and
`CLAUDE_CODE_PERMISSION_MODE=acceptEdits` restores the looser configuration.
Set `CLAUDE_CLI_PATH` when running under launchd or systemd, which do not
inherit the `~/.local/bin` the CLI usually installs into.

`costUSD` and the duration of every identify are logged at info level, so
you can watch what an evaluation session is costing while it runs. As with every
other provider, a failure here degrades to the manual-entry path rather than
failing the capture.

Homebox itself needs `HBOX_AUTH_API_KEY_PEPPER` (32+ chars) for API keys to
work; docker-compose.yml sets a dev value you must change.

## Container images

Published to `ghcr.io/auzroz/boxwright` for **linux/amd64 and linux/arm64**, by
`.github/workflows/release.yml`. arm64 is not a nicety: the cluster this runs on
is mostly arm64 Pis, and a manifest missing that half pushes and reports success
just the same — so the workflow inspects what it published and fails if either
architecture is absent.

    docker pull ghcr.io/auzroz/boxwright:edge      # latest manual build
    docker pull ghcr.io/auzroz/boxwright:0.1.0     # a release

A `v*.*.*` tag publishes a release; **Run workflow** publishes `edge` and a
sha-tagged image, which is how the pipeline gets exercised long before there is
anything to release. A pre-release tag (`v0.1.0-rc1`) deliberately does not
claim `latest`.

The build stage cross-compiles rather than being emulated per target — the
binary is stdlib-only with `CGO_ENABLED=0`, so there is nothing to gain from
running the Go compiler under QEMU.

## Deploying

`deploy/kubernetes/boxwright.yaml` is a commented example: one replica
replaced with `Recreate` (the in-flight dedupe claim is per process), non-root,
read-only root filesystem, `/healthz` probes, and a Tailscale ingress for the
certificate iOS wants. `GET /api/v1/status` reports the running version.

`HOMEBOX_WATCH` (on by default) opens a WebSocket from the backend *out* to
Homebox, so the ingress in front of Boxwright needs nothing for it. What it
does need is egress to Homebox that tolerates a long-lived connection, and an
`Upgrade`-passing proxy if `HOMEBOX_BASE_URL` goes through one rather than
the in-cluster Service. `/healthz` stays 200 either way, so a Homebox outage
never fails the probes.

## Known limitations

- **Identification is not fast.** About five seconds per object found: 3s for
  a single item, 73s for a shelf of sixteen. Cost tracks the same thing —
  ~$0.007 to ~$0.022 a photo on Sonnet 5 — because 77% of the bill is output
  tokens. Park a slow capture rather than waiting for it.
- **Parked captures only progress while the app is open.** React Native has no
  reliable background execution. The photo is durable, so nothing is lost; work
  resumes next time you open Boxwright.
- **Per-item crops need a model that can ground.** See the table above. Where
  they cannot, every card shows the whole photo.
- **The scoring weights are unvalidated.** `CategoryMatch 5.0`,
  `EmptyBonus 1.75`, `MinViableScore 2.75` and the rest were reasoned about,
  not measured against anyone's judgement. `cmd/corpus` is the harness for
  fixing that and skips cleanly with no data.
- **The engine has no spatial model.** `gridX`/`gridY` are stored and unused;
  there is no front-of-unit or floor-level reasoning, only an `access` bucket.
- **The change feed is an accelerator, not a guarantee.** The box index is a TTL
  cache, patched in place on Boxwright's own writes and marked stale as soon as a
  change feed event says something else happened (`HOMEBOX_WATCH`) — rebuilt
  there and then when that Homebox has been read within `BOX_CACHE_TTL`, and
  otherwise left for whoever asks next. But that
  feed has no resume cursor: whatever changed while the socket was down is
  announced to nobody, so an edit made in Homebox's own UI is still worst-case
  invisible until `BOX_CACHE_TTL` expires.

## Architecture

Go backend (stdlib only) + bare React Native app. See `docs/ARCHITECTURE.md`
and `CLAUDE.md` (this repo is set up for [Claude Code](https://claude.com/claude-code)
agentic development).

## Contributing

AGPL-3.0, no CLA, DCO sign-off (`git commit -s`). See CONTRIBUTING.md.
We will not relicense this project away from an OSI-approved license.
