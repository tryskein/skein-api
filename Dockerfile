# syntax=docker/dockerfile:1.7
#
# skein-api — Go HTTP service.
#
# Multi-stage build:
#   1. builder  — golang:1.26-alpine, downloads modules and produces a static
#                 binary (CGO disabled, ldflags="-w" strips DWARF for ~20% size
#                 reduction while keeping the symbol table for stack traces).
#   2. runtime  — alpine:3.21 with the binary plus the diagnostic utilities
#                 mandated by the project guidelines (curl, ping, psql, dig, nc).
#                 Runs as non-root user appuser (uid 1000).
#
# golang-migrate is installed in the runtime stage so the same image can be
# used for production migrations via `docker run --entrypoint migrate ...`.
# In Development the binary applies migrations in-process at startup using
# the migrate library API (see cmd/api/main.go).

# ============ Stage 1: builder ============
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Download module deps in a separate layer so they're cached when only source
# files change.
COPY src/go.mod src/go.sum ./
RUN go mod download

# Compile the API binary. Static linking via CGO_ENABLED=0 plus -ldflags="-w"
# (drop DWARF; keep symbol table) yields a self-contained executable that runs
# on a bare Alpine image.
COPY src/ ./
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w" \
    -o /app/server \
    ./cmd/api/

# ============ Stage 2: runtime ============
FROM alpine:3.21

# Diagnostic utilities required by the project guidelines so operators can
# debug a live container: HTTP probes, network reach, DB connectivity, DNS
# resolution, and TCP port checks.
RUN apk add --no-cache \
    curl \
    iputils-ping \
    postgresql17-client \
    bind-tools \
    netcat-openbsd \
    ca-certificates \
    tzdata

# golang-migrate CLI — used in production for explicit "run migrations once"
# invocations via `docker run --rm --entrypoint migrate <image> -path ...`.
# Installed before USER switch so the binary lives at /usr/local/bin/migrate
# and is owned by root (read+execute for everyone).
ARG MIGRATE_VERSION=4.20.1
RUN ARCH=$(uname -m) && \
    case "$ARCH" in \
        x86_64)  ARCH="amd64" ;; \
        aarch64) ARCH="arm64" ;; \
    esac && \
    curl -fsSL "https://github.com/golang-migrate/migrate/releases/download/v${MIGRATE_VERSION}/migrate.linux-${ARCH}.tar.gz" \
    | tar -xz -C /usr/local/bin/ migrate

# Non-root user — running as root inside containers is forbidden by org policy
# and breaks Kubernetes PodSecurity restricted profiles.
RUN addgroup -g 1000 appgroup && \
    adduser -u 1000 -G appgroup -D appuser

COPY --from=builder /app/server /app/server

# Migration SQL files — read by both the in-process migrator (Development)
# and the migrate CLI (Production). The path matches the relative file://
# URL used in cmd/api/main.go — Development runs from /app/, so the loader
# resolves "file://./migrations" → /app/migrations.
COPY src/migrations /app/migrations

# The license text travels with every copy of the image (BSL 1.1 requires it).
COPY LICENSE /licenses/LICENSE

WORKDIR /app

USER appuser

# Default port; can be overridden via the PORT env var without rebuilding.
ENV PORT=5100
EXPOSE 5100

ENTRYPOINT ["/app/server"]
