# Release plan: v0.1.0

Last revised 2026-09-28. This is the working plan CLAUDE.md's roadmap points
at; edit it as things land rather than writing a new one.

## What v0.1.0 is

The first tagged release, and the one that starts the awesome-selfhosted
four-month eligibility clock. It ships three things together:

1. **The backend image**, `ghcr.io/auzroz/boxwright:0.1.0`, linux/amd64 and
   linux/arm64, built by `.github/workflows/release.yml` from a `v0.1.0` tag.
2. **The iOS app on TestFlight, internal testing.** Not the public App Store,
   and not external TestFlight; see decision 3 below for why.
3. **Docs a stranger can self-host from**: README quick start through to a
   phone filing its first item, the Kubernetes example, and a privacy policy.

Android is not in v0.1.0. Nothing done for iOS closes it off: the app has no
iOS-only code, and every native module chosen supports Android. What it lacks
is an `android/` project, generated from the React Native template when that
work starts.

## Where things stand

Done, with the evidence:

| | |
|---|---|
| Backend builds, vets, tests (race) | `ci.yml`, every PR |
| Homebox v0.26.x API verified live | `docs/HOMEBOX.md`, `internal/homebox/testdata/` |
| Attachment upload in `/catalog` | `internal/api/catalog.go` |
| Offline capture queue | `app/src/offline.ts`, 133 tests in `app/src` |
| Park a photo, identify in the background, review later | `offline.ts` pending list, `pending.test.ts` |
| Paste a copied image; per-item crops drawn from the shared photo | `App.tsx`, `src/crop.ts` |
| Bulky items go to an area, never a box (`NoPlace` instead of a new box) | `internal/placement/engine.go` |
| Box index rebuilt on Homebox's change feed (`HOMEBOX_WATCH`) | `internal/api/watch.go`, `internal/homebox/events.go` |
| Multi-arch image, both arches verified in the manifest | `release.yml` |
| Multi-item capture, user-chosen placement locations | commits `8746bc2`, `7d3a405` |
| `GET /api/v1/status`: version, API generation, identification on/off, Homebox reachability | `internal/api/status.go` |
| Server address and token set **in the app**, stored in the Keychain | `app/src/connection.ts`, `ConnectionScreen.tsx` |
| Switching server refuses to carry queued captures to a different inventory | `offline.switchConnection` |
| **Off Expo**: bare React Native, committed Xcode project | `app/ios/`; decision 8 |
| iOS config: bundle id, ATS, local-network prompt, export compliance, privacy manifest, icon | `app/ios/Boxwright/`, checked by `npm run check:ios` in CI |
| iOS compiles: pod install + unsigned Release build, every push touching `app/` | `.github/workflows/ios.yml` (macOS); also built and launched on the maintainer's Mac (Xcode 27, Simulator) 2026-09-28 |
| `Podfile.lock` committed | `app/ios/Podfile.lock` |
| TestFlight upload from the maintainer's Mac, no build service | `app/ios/fastlane/Fastfile` |
| Choose a photo from the library (system picker, no permission) | `App.tsx` |
| Kubernetes example | `deploy/kubernetes/boxwright.yaml` |

The server-address change was the one hard blocker for any iOS build that
leaves the developer's machine. Until then the address and token were
build-time variables inlined into the JavaScript bundle: a TestFlight build
would have been welded to one server, with that server's token readable by
anyone who unzipped the app.

## Gates

Everything here is required for the `v0.1.0` tag. Tick in the PR that does it.

### Backend

- [ ] **Run `edge` in-cluster against the real Homebox for a week of real
      captures.** Start from `deploy/kubernetes/boxwright.yaml`. The
      maintainer's cluster runs it from home-ops
      (`kubernetes/apps/documents/boxwright`, auzroz/home-ops#288), pinned to
      the `sha-<commit>` tag of a `main` build, never a floating tag. This is the
      end-to-end test nothing else provides: live Homebox, real photos, real
      signal in a real storage unit.
- [x] **Scripted end-to-end pass, API half** (`make e2e`,
      `backend/e2e/e2e_test.go`): the real server binary against a real
      Homebox v0.26.2 with a stub vision model -- `/api/v1/status` (token
      refused without, Homebox reachable with), the change feed reaching
      `connected`, choosing locations (read back as `boxwrightPlacement`),
      identify (the model receives exactly the uploaded photo, as
      image/jpeg), recommend, catalog with the photo attached, a partial
      failure resent with the same entry ids and deduplicated by
      `boxwrightKey`, an identical resend after a "lost" response, and the
      nil-UUID box refused. Every outcome is read back from Homebox itself,
      not taken from Boxwright's reply. Run 2026-09-28 against the v0.26.2
      Linux binary; runs on every PR as the `e2e` job in `ci.yml` against the
      v0.26.2 image. It found one bug on its first run (the nil UUID, fixed).
- [ ] **End-to-end pass, app half**, on the device: configure the app,
      choose locations, capture one item and one multi-item photo, file
      both, confirm the entities and the photo attachment in Homebox, then
      repeat in airplane mode and confirm the queue drains on return. This
      is the device pass below, run against the same disposable Homebox.
- [ ] **Flip README's "Status: pre-release scaffold. Not yet usable end to
      end."** only once both of the above are done.
- [ ] `CHANGELOG.md` with a 0.1.0 section.
- [ ] Tag `v0.1.0-rc1` first. The workflow deliberately does not move
      `latest` for a pre-release, so this is a free rehearsal of the real tag.

### iOS

- [x] **Apple Developer Program membership**: individual (decision 2).
- [x] **Bundle id `app.boxwright`**, confirmed (decision 1). Permanent once
      an App Store Connect record exists.
- [x] **`ios.yml` green on macOS.** The first real compile of the native
      project; nothing in the Linux container this was written in can build
      iOS. Green on every push since 2026-09-28, with `Podfile.lock`
      committed, so CI now fails if it drifts.
- [x] **First local run.** `cd app && npm ci && bundle install && (cd ios &&
      bundle exec pod install)`, open `ios/Boxwright.xcworkspace`, choose your
      team under Signing & Capabilities for the Boxwright target, and run on
      your iPhone. Xcode registers `app.boxwright` on the account the first
      time; that is the moment the bundle id becomes yours.
      Done 2026-09-28 from the command line (`xcodebuild
      -allowProvisioningUpdates DEVELOPMENT_TEAM=...`, then `devicectl`): a
      Release build opened on the server screen on an iOS 27.2 iPhone. It
      found that the iOS 27 SDK will not launch an app without the UIScene
      life cycle ("UIScene life cycle is required for apps built with this
      SDK"): fixed before the public release (AppDelegate.swift's
      SceneDelegate). `ios.yml` compiles on an older Xcode and could not have
      seen it.
- [x] **Create the app record** in App Store Connect (My Apps > +, bundle id
      `app.boxwright`, SKU anything) and an **App Store Connect API key**
      (Users and Access > Integrations, App Manager). Keep the `.p8` outside
      the repo.
- [x] **`cd app/ios && bundle exec fastlane beta`**: archives, signs with
      Xcode's automatic signing through the Apple ID signed in to Xcode (App
      Store distribution uses a cloud-managed certificate, which an App
      Manager API key may not sign with), takes the next build number from
      TestFlight and uploads with the API key. Key, Issuer ID and team come
      from the environment or a gitignored `app/ios/fastlane/.env` -- see the
      Fastfile's header. The marketing version is `MARKETING_VERSION` in the
      Xcode project (0.1.0). First uploads, 2026-09-29: build 1 refused at
      ingestion (ITMS-90683, the photo-library purpose string the image
      picker's linked code requires); build 2 accepted, no warnings. The lane
      now waits for that verdict and fails with Apple's words.
      `fastlane archive` builds and signs without uploading.
- [x] **Read the email Apple sends after the first upload.** Missing
      privacy-manifest reasons arrive there as ITMS-91053 warnings rather than
      as a failed upload. The app's manifest declares what
      `react-native-file-access` touches; MMKV and the image picker ship their
      own. Any warning is a line to add to `PrivacyInfo.xcprivacy` and to
      `scripts/check-ios-config.sh`. The lane also prints the warnings App
      Store Connect records on the upload; build 2 had none.
- [ ] **App Store Connect privacy answers: "Data Not Collected".** Photos and
      item details go only to the server the user runs; the developer never
      receives them. Privacy policy URL: `docs/PRIVACY.md` on GitHub.
- [ ] **Device pass**, on a real iPhone from the TestFlight build, not a Debug build:

  | Check | Expect |
  |---|---|
  | Fresh install | Server settings screen first, nothing else reachable |
  | `http://<LAN IP>:8080` | Works; settings screen warns the token is unencrypted |
  | `http://100.x.y.z:8080` (Tailscale IP) | Works (an IP literal is local to ATS) |
  | `http://box.<tailnet>.ts.net` | Refused by iOS, and the settings screen said it would be before saving |
  | `https://boxwright.<tailnet>.ts.net` (Tailscale Ingress) | Works, no warnings |
  | Local-network permission denied | Settings test says it cannot reach the server; granting it in Settings recovers |
  | Wrong token / Homebox down / `AI_PROVIDER=none` | Each gets its own sentence on the test result |
  | Camera permission denied | Alert with Open Settings, no crash |
  | Choose an existing photo | System picker, no permission prompt; the photo is filed with the item |
  | Paste a copied image (long-press an image in Safari, Copy) | iOS "Allow Paste" prompt only on tap; the image is filed like a photo |
  | Take a shelf of several items, park it, open it from the list later | Identified in the background; per-item thumbnails line up with their items |
  | Take photo on a LiDAR iPhone | Boxwright's own camera opens, with "Use the system camera" one tap away; a phone without LiDAR gets the system camera |
  | A boxed item (e.g. a shoebox) at ~0.8 m, 45 degrees | Review card says "measured with LiDAR"; within ~1.5 cm of a tape measure on each side |
  | A shelf of several items, each located by the model | Each measured separately; an item the model did not locate keeps its estimate |
  | Record container sizes: tick several, "New type...", "27 gal", inside size | Type, 102 L and the inside size show on every ticked container, and in Homebox |
  | File into a container of known size | Done screen asks "How full is it now?"; an answer is saved and wins over Boxwright's own estimate |
  | Measure, over an open container of known inside size | Guide and tilt shown; refuses past 30 degrees from straight down; within ~10 points of what you see |
  | A photo's depth file after review | Gone from the app's captures directory once the capture is filed, queued or discarded |
  | Photo filed to Homebox | 1024px on its longest edge, no location in its EXIF |
  | Capture in airplane mode, file to a cached box, reconnect, foreground | Queue drains; item in Homebox with its photo |
  | Change server with a capture queued | Refused with the reason; token-only change allowed |
  | Force-quit and relaunch | Server settings and queue both survive |
  | Fresh install | Welcome, then the four setup steps; Continue on Connect only after a passing test; no "Save Password?" sheet |
  | Upgrade over the previous build | Straight to Home; never the welcome |
  | Try the demo (from Welcome) | DEMO strip on every screen; photo, review, where they go, filed, fill answer all work with no server; airplane mode changes nothing |
  | Leave the demo with real captures queued | The real queue and parked photos are exactly as they were; nothing from the demo is in Homebox |
  | Homebox theme changed in the web UI | The app's accent follows on the next foreground (within 10 min); off in Settings keeps the amber |

  The LiDAR rows are the first real test of the camera module and the
  measurement: until now both ran only against synthetic depth frames.

  The two Tailscale-IP and MagicDNS rows are the ones not settled by reading
  Apple's documentation; the settings-screen wording in
  `connection.transportWarnings` assumes the outcome shown and must be
  corrected if a device disagrees.

- [ ] Replace the placeholder icon
      (`app/ios/Boxwright/Images.xcassets/AppIcon.appiconset/AppIcon-1024.png`,
      generated: a grey isometric box on near-black). It passes App Store validation (1024 px,
      RGB, no alpha), so this is not blocking TestFlight, only the public
      listing.

## Decisions

1. **Bundle id: `app.boxwright`. Decided.** Reverse-DNS of `boxwright.app`,
   which was free when the name was chosen. Apple does not check domain
   ownership, but a bundle id matching a domain the project controls avoids a
   collision later. Fixed from the first upload on.

2. **Apple account: individual. Decided.** The maintainer is enrolled as an
   individual. What that means in practice:
   - The seller shown on the App Store and to TestFlight testers is the
     maintainer's legal name, not "Boxwright". That is normal for an
     individual developer and needs no action.
   - Internal TestFlight testers are added as App Store Connect users under
     Users and Access; they do not join the developer team or get signing
     access.
   - Xcode's automatic signing creates and renews the certificate and
     profile on this account; `fastlane beta` passes
     `-allowProvisioningUpdates` so it can do that during an archive.
   - Moving to an organisation later (for a public listing under the project's
     name) is Apple's app-transfer process, which keeps the bundle id and
     reviews but has its own eligibility rules; nothing in the repo changes.

3. **v0.1.0 is internal TestFlight only** (recommended). External TestFlight
   and the App Store both go through Apple review, and a reviewer must be able
   to use the app. That no longer needs a public backend: **the app has a
   demo built in** (src/demo/) -- a sample inventory on the phone, reached
   from "Try the demo" on the first screen, that never goes online and is
   deleted on leaving. Internal testing covers up to 100 people on the
   developer's App Store Connect team with no review at all. Revisit for v0.2.

   Review notes, for when it is submitted:

   > Boxwright files items into Homebox, a self-hosted inventory system, through
   > a small server the user runs themselves; there is no account or cloud
   > service. To review without one, tap **Try the demo** on the first screen.
   > The demo uses a sample inventory stored on the device, needs no network,
   > and identifies the same three example items in any photo. "Choose an
   > existing photo" works on any device. Leave the demo from the banner at the
   > top. LiDAR measurement appears only on iPhones with LiDAR; elsewhere sizes
   > are estimated and can be typed.

4. **No over-the-air updates.** An update service checks a vendor's servers
   on every launch, which is a phone-home by principle 5 whatever it carries.
   Every change ships as a TestFlight build.

5. **No crash reporting SDK.** TestFlight already collects crash reports from
   testers who opt in at the OS level, which is Apple's consent flow rather
   than ours. Nothing is added to the app.

6. **Light appearance only for v0.1.0.** Every colour in the app assumes a
   white background; `UIUserInterfaceStyle: Light` keeps a phone in dark mode
   from putting white status-bar text on it. A dark palette is post-0.1.

7. **HTTPS is expected, http is tolerated on the local network.** App
   Transport Security stays on with only `NSAllowsLocalNetworking`. The
   alternative, `NSAllowsArbitraryLoads`, would let the app send its token in
   the clear to any hostname and needs a justification in App Review. The
   Kubernetes example and README show the Tailscale route to a certificate,
   which is less work than explaining the exception.

8. **Off Expo before the first release. Decided 2026-09-28.** The goal is that
   no vendor but Apple sits in the release path: no Expo account, no EAS
   build or signing service, no Expo modules. The app is bare React Native
   0.86 (the version it already ran, so this was a change of platform, not
   also an upgrade) with a committed Xcode project, built and signed on the
   maintainer's Mac and uploaded with fastlane. Each Expo module was replaced
   (ARCHITECTURE.md lists the new ones and why); the only behavioural
   change is that both queues now live in MMKV rather than JSON files,
   chosen because it keeps `enqueue` synchronous. Two consequences: a Mac is
   required to build the app at all, and Android, when it comes, needs its
   own committed `android/` project from the React Native template.

## After v0.1.0 (ordered)

1. External TestFlight / App Store: the review demo exists (decision 3).
2. Dark palette (decision 6).
3. Android: generate `android/` from the React Native 0.86 template, package
   `app.boxwright`, then Play internal testing.
4. Optional LLM re-ranker behind the `Reranker` interface in placement.
5. Spatial unit map screen (grid coords from location custom fields).
