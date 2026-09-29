# Contributing

Thanks for helping. Ground rules:

## License and sign-off

- The project is AGPL-3.0. There is no CLA and there will be no CLA; we will
  not relicense away from an OSI-approved license.
- Every commit needs a Developer Certificate of Origin sign-off:
  `git commit -s` (adds `Signed-off-by: Your Name <you@example.com>`). CI
  checks every commit in a PR and fails without it, so amend rather than
  discovering it at review.
  This certifies you have the right to submit the code under AGPL-3.0.

## Development

Toolchain: **Go 1.23+** and **Node 22** (what CI uses), React Native 0.86.3; the iOS build needs Xcode on macOS.

- Backend: Go, stdlib only. New dependencies need a justification in the PR
  description. `make test` must pass — that is `gofmt -l` (any output fails),
  `go vet`, and `go test ./...`. **CI additionally runs `go test -race`**, so
  run that yourself before opening a PR.
- App: bare React Native (no Expo), TypeScript strict with
  `noUncheckedIndexedAccess`. `npm run typecheck`, `npm test` (or `make
  app-test`) and `npm run check:ios` must pass; the iOS compile runs in CI on
  macOS (`.github/workflows/ios.yml`). `make test` is backend-only on purpose,
  so it still works in a checkout with no `node_modules`.
- Exercising the app: `cd app && npm ci && (cd ios && bundle exec pod install)
  && npm run ios`. **The iOS simulator has no camera**, so the camera and
  paste flows need a device; "Choose an existing photo" is the way in on the
  simulator.
- The test doubles in `app/test/` are hand-written stand-ins for
  `react-native-mmkv` and `react-native-keychain`, which reach native code at
  import and cannot load in Node. If you add a call to either module, extend
  the fake to match the REAL typings — bugs have been found in the fakes rather
  than the code.
- The JSON contracts in `backend/internal/placement/engine.go` and
  `app/src/types.ts` are mirrors; change both in one commit. That now covers
  `ItemDraft` (including `Region` and `Bulky`), `Box` (including `IsArea`),
  `Recommendation` (including `NoPlace`), and every request and result type in
  `internal/api`. A field added to the identify prompt must ALSO be added to
  the Anthropic output schema in `internal/ai/anthropic.go`, which closes
  `additionalProperties` — a test enforces that the two agree, because a field
  the schema omits simply cannot be answered.
- Placement engine changes require tests. Scoring weight changes require a
  test demonstrating the intended behavior change.

## Container images and releases

`docker build` is never run on a dev machine here, so **CI is the only Docker
oracle** — treat a red `docker` job as a real break.

A `v*.*.*` tag runs `.github/workflows/release.yml`, which publishes a
multi-arch image (`linux/amd64` and `linux/arm64`) to
`ghcr.io/auzroz/boxwright`. **arm64 is not optional**: the deployment target is
mostly arm64 Pis, and a manifest missing that half pushes and reports success
just the same — so the workflow inspects what it published and fails if either
architecture is absent. Run it by hand (**Run workflow**) to publish `edge`
whenever you want to exercise the pipeline.

## Non-negotiables

- No telemetry, analytics, or phone-home of any kind.
- No AI call may be required for a core flow (`AI_PROVIDER=none` stays fully
  usable).
- Homebox remains the system of record; no durable inventory state elsewhere.

## AI-assisted contributions

This repo is set up for Claude Code (see CLAUDE.md). AI-assisted PRs are
welcome; you are responsible for reviewing and testing what you submit, and
the DCO sign-off applies to it like any other contribution.
