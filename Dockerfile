# Boxwright backend. Stdlib-only Go, so no module downloads are needed.
#
# The build stage pins itself to the BUILD platform and cross-compiles, rather
# than being emulated once per target. Three of the four nodes this is deployed
# to are arm64 Pis, so every release builds linux/amd64 and linux/arm64; running
# the Go compiler under QEMU for the arm64 half would take minutes for a binary
# that has no cgo and no dependencies and cross-compiles for free.
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS
ARG TARGETARCH
# Reported by GET /api/v1/status. The release workflow passes the tag; a local
# `docker build` reports "dev".
ARG VERSION=dev
WORKDIR /src
COPY backend/ .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/boxwright ./cmd/server

FROM alpine:3.20
# A CGO_ENABLED=0 binary carries no CA bundle of its own, so without this every
# HTTPS call fails with "x509: certificate signed by unknown authority" -- which
# is the entire AI_PROVIDER=openai/anthropic path.
RUN apk add --no-cache ca-certificates
RUN adduser -D -u 10001 boxwright
COPY --from=build /out/boxwright /usr/local/bin/boxwright
USER boxwright
EXPOSE 8080
ENTRYPOINT ["boxwright"]
