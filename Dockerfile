# Multi-stage build for the Runkite control plane (Go).
# Uses modernc.org/sqlite (pure Go) so CGO is not required.
# Final stage runs as a non-root user (uid 65532).

FROM golang:1.25.13-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN GIT_COMMIT=$(cat .git/refs/heads/main 2>/dev/null | cut -c1-7 || echo "docker") && \
    BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ") && \
    CGO_ENABLED=0 go build \
      -ldflags "-X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X main.BuildTime=${BUILD_TIME}" \
      -o runkite ./cmd/

# Runtime base is independent of the golang:*-alpine builder image.
# Keep this on a supported Alpine line so Trivy can assess CVEs (3.20
# reached EOL 2026-04-01 and produced a false-clean scan).
FROM alpine:3.24
RUN apk add --no-cache ca-certificates wget && \
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
