# syntax=docker/dockerfile:1.7
#
# skein-api — Go HTTP service.
#
# Multi-stage build:
#   1. builder  — golang:1.27-alpine, downloads modules and produces a static
#                 binary (CGO disabled, ldflags="-w" strips DWARF for ~20% size
#                 reduction while keeping the symbol table for stack traces).
#   2. runtime  — alpine:3.24 with the binary plus the diagnostic utilities
#                 mandated by the project guidelines (curl, ping, psql, dig, nc).
#                 Runs as non-root user appuser (uid 1000).
#
# golang-migrate is installed in the runtime stage so the same image can be
# used for production migrations via `docker run --entrypoint migrate ...`.
# In Development the binary applies migrations in-process at startup using
# the migrate library API (see cmd/api/main.go).

# ============ Stage 1: builder ============
FROM golang:1.27-alpine AS builder

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
FROM alpine:3.24

# Diagnostic utilities required by the project guidelines so operators can
# debug a live container: HTTP probes, network reach, DB connectivity, DNS
# resolution, and TCP port checks.
RUN apk add --no-cache \
    curl \
    iputils-ping \
    postgresql18-client \
    bind-tools \
    netcat-openbsd \
    ca-certificates \
    tzdata

# golang-migrate CLI — used in production for explicit "run migrations once"
# invocations via `docker run --rm --entrypoint migrate <image> -path ...`.
# Installed before USER switch so the binary lives at /usr/local/bin/migrate
# and is owned by root (read+execute for everyone). The archive is checked
# against a pinned sha256 before it is unpacked (release assets can be
# replaced); bumping MIGRATE_VERSION means updating both sums from the
# release's sha256sum.txt. An unknown architecture fails the build.
ARG MIGRATE_VERSION=4.20.1
ARG MIGRATE_SHA256_AMD64=b1e13121d4178b7804a9fb29dca62d57b732390f7e5ae33f2cd65e9b192e1bd4
ARG MIGRATE_SHA256_ARM64=03ea70d9ba535b68fe03602120d1856a567cd0fa2fbca3748ba8070e9d4b8f60
RUN set -eu; \
    case "$(uname -m)" in \
        x86_64)  ARCH="amd64"; SUM="$MIGRATE_SHA256_AMD64" ;; \
        aarch64) ARCH="arm64"; SUM="$MIGRATE_SHA256_ARM64" ;; \
        *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;; \
    esac; \
    archive="/tmp/migrate.linux-${ARCH}.tar.gz"; \
    curl -fsSL -o "$archive" "https://github.com/golang-migrate/migrate/releases/download/v${MIGRATE_VERSION}/migrate.linux-${ARCH}.tar.gz"; \
    printf '%s  %s\n' "$SUM" "$archive" | sha256sum -c -; \
    tar -xzf "$archive" -C /usr/local/bin/ migrate; \
    rm -f "$archive"

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
