# ─────────────────────────────────────────────────────────────────────────────
#  surebetter-api-gateway — Dockerfile
#  Go: WebSocket hub, delta push, HTTP snapshot fallback
#
#  Stages:
#    builder      Compile Go binary with all optimizations
#    development  Installs air for hot-reload (used by docker-compose.override.yml)
#    production   Minimal scratch image
#
#  Note on go.sum: COPY go.mod go.su[m] ./ uses a glob so the COPY does not
#  fail on a fresh repo where go.sum has not been generated yet.
# ─────────────────────────────────────────────────────────────────────────────

# ── Stage 1: Builder ──────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

COPY go.mod go.su[m] ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" \
    -o /bin/api-gateway \
    ./cmd/gateway


# ── Stage 2: Development ──────────────────────────────────────────────────────
FROM golang:1.25-alpine AS development

RUN apk add --no-cache git ca-certificates tzdata curl

# Pinned: air v1.67.2+ requires go >= 1.26 and this image is go 1.25 with
# GOTOOLCHAIN=local, so @latest breaks the build. Bump together with the base image.
RUN go install github.com/air-verse/air@v1.67.1

WORKDIR /app

COPY go.mod go.su[m] ./
RUN go mod download

COPY . .

EXPOSE 8080

CMD ["air", "-c", ".air.toml"]


# ── Stage 3: Production ───────────────────────────────────────────────────────
FROM scratch AS production

COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /bin/api-gateway /api-gateway

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/api-gateway", "-healthcheck"]

ENTRYPOINT ["/api-gateway"]