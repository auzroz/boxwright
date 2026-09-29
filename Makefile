.PHONY: test vet fmt fmtcheck build run docker app-install app-typecheck app-test bootstrap bootstrap-apply

# -tags e2e so the end-to-end harness is vetted too; it is not run here.
vet:
	cd backend && go vet ./... && go vet -tags e2e ./e2e

# gofmt has no exit code of its own; -l printing anything is the failure.
fmtcheck:
	@out="$$(cd backend && gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

fmt:
	cd backend && gofmt -w .

# CONTRIBUTING.md makes `make test` the contributor gate, so it must run
# everything CI runs for the backend -- otherwise local and CI diverge.
test: fmtcheck vet
	cd backend && go test ./...

# Build once, then run the binary -- NOT `go run`, which recompiles on every
# start and holds the Go toolchain resident while the server runs. On a 16 GB
# machine already into swap, a dozen restarts in a session is enough to get
# things killed by the memory watchdog. The built binary is stdlib-only and
# CGO_ENABLED=0, so it costs about 10 MB resident and nothing to restart.
build:
	cd backend && CGO_ENABLED=0 go build -trimpath -o boxwright ./cmd/server

run: build
	cd backend && ./boxwright

docker:
	docker build -t boxwright:dev .

# The scripted end-to-end gate (docs/RELEASE.md) against a real Homebox
# v0.26.x: `docker compose up -d homebox`, or any instance that allows
# registration and has HBOX_AUTH_API_KEY_PEPPER set. It builds and runs the
# real server binary with a stub vision model; see backend/e2e/e2e_test.go.
E2E_HOMEBOX_URL ?= http://127.0.0.1:7745
e2e:
	cd backend && E2E_HOMEBOX_URL=$(E2E_HOMEBOX_URL) go test -tags e2e -count=1 -v ./e2e

app-install:
	cd app && npm ci

app-typecheck:
	cd app && npm run typecheck

# Deliberately NOT a dependency of `make test`. That gate is backend-only by
# design: it must run without node_modules present, which is the state a Go
# contributor's checkout is in.
app-test:
	cd app && npm test

# Create the storage hierarchy in Homebox from storage.txt. Boxwright cannot
# cold-start itself: with no containers there is nothing to recommend against.
bootstrap:
	cd backend && go run ./cmd/bootstrap -f ../storage.txt

bootstrap-apply:
	cd backend && go run ./cmd/bootstrap -f ../storage.txt -apply
