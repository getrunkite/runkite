# Multi-stage build for the Runkite control plane (Go).
# Uses modernc.org/sqlite (pure Go) so CGO is not required.
# Final stage runs as a non-root user (uid 65532).

FROM golang:1.25.13-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# .dockerignore excludes .git, so the image cannot read the SHA from
# the repo. Callers (release.yml) pass GIT_COMMIT; local `docker build`
# without the arg stamps "docker".
ARG GIT_COMMIT=docker
ARG BUILD_TIME
RUN BUILD_TIME="${BUILD_TIME:-$(date -u +"%Y-%m-%dT%H:%M:%SZ")}" && \
    CGO_ENABLED=0 go build \
      -ldflags "-X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X main.BuildTime=${BUILD_TIME}" \
      -o runkite ./cmd/

# Runtime base is independent of the golang:*-alpine builder image.
# Keep this on a supported Alpine line so Trivy can assess CVEs (3.20
# reached EOL 2026-04-01 and produced a false-clean scan).
# Digest is the multi-arch index for alpine:3.24 (3.24.1 as of 2026-06-16).
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
# `apk add` only installs the packages it's given -- it does not touch
# already-present ones. The base image's own baked-in packages (libssl3,
# libcrypto3, ...) age between when Docker Hub last rebuilt this tag and
# whenever this Dockerfile actually runs, so `apk upgrade` first pulls
# any fix already published for the 3.24 branch (found live: a
# base-image-only openssl CVE failed the image scan despite the Go
# binary itself being clean).
RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates wget && \
    addgroup -g 65532 -S runkite && \
    adduser -u 65532 -S -G runkite -H -D runkite

COPY --from=builder /app/runkite /usr/local/bin/runkite
COPY --from=builder --chown=runkite:runkite /app/examples /app/examples

# SQLite default path when POSTGRES_DSN/MYSQL_DSN/MONGO_URI are unset --
# /tmp is world-writable so the non-root user can create the DB file
# without a separate volume. Compose stacks that use Postgres ignore this.
ENV DATABASE_PATH=/tmp/runkite.db
WORKDIR /app

USER runkite

EXPOSE 2026 50051
ENTRYPOINT ["runkite"]
CMD ["serve"]
