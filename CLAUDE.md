# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Go WebSocket gateway. Subscribes to `arb.opportunities` on NATS JetStream, maintains in-memory state of all current arbitrage opportunities, and pushes delta patches to browser clients via WebSocket.

## Structure

All application code lives in a **single file**: `cmd/gateway/main.go` (no internal packages).

Key types in that file:
- `Config` — env vars loaded at startup
- `StateHub[T]` — central state of one channel: client map + item state + snapshot cache, parameterized by a `hubSpec` (key func, sport func, removed func, patch path prefix). `Hub` = `StateHub[ArbOpportunity]` (`/ws`, `arb.opportunities`, `/arb/`); `ValueHub` = `StateHub[ValueBet]` (`/ws/value`, `value.opportunities`, `/value/`)
- `ValueBet` — decodes only `id`/`sport`/`removed`; marshals the engine's original JSON verbatim
- `Client` — single WebSocket connection with a send channel
- `ArbOpportunity` / `StakeAllocation` — mirrors the Rust arb-engine structs
- `WSMessage` / `PatchOp` — WebSocket protocol types (RFC 6902 JSON Patch)

## Running & Building

All Docker operations are run from `../surebetter-common/`:
```bash
task build-service SERVICE=api-gateway
task restart-service SERVICE=api-gateway
task logs-service SERVICE=api-gateway
```

Direct Go commands (for local dev without Docker):
```bash
go build ./cmd/gateway
go test ./...
```

Hot-reload in Docker dev mode uses `air` (configured in `.air.toml`): rebuild triggers on any `.go` save, ~3s reload.

## HTTP Endpoints

| Route | Purpose |
|-------|---------|
| `GET /ws` | WebSocket upgrade — streams arb deltas to clients |
| `GET /api/arb/snapshot` | REST fallback — returns full current state as JSON |
| `GET /ws/value` | WebSocket upgrade — streams value-bet deltas (independent hub; same protocol, path prefix `/value/{id}`) |
| `GET /api/value/snapshot` | Full current value-bet state as JSON (`{id: ValueBet}`) |
| `GET /health` | `{"status":"ok","service":"api-gateway","version":"dev"}` |
| `GET /metrics` | Prometheus counters: `gateway_ws_clients_connected`, `gateway_messages_sent_total` |

## WebSocket Protocol

1. On connect: full snapshot → `{"type":"snapshot","data":{...}}`
2. On each NATS message: RFC 6902 patch → `{"type":"patch","ops":[{"op":"replace","path":"/arb/{eventID}:{marketType}:{line:.2f}","value":{...}}]}`
3. Keepalive: `{"type":"ping"}` every 30s

Patches are **batched** in a `BROADCAST_BATCH_MS` (default 50ms) window before broadcasting.

## Environment Variables

| Variable | Default | Notes |
|----------|---------|-------|
| `NATS_URL` | `nats://localhost:4222` | |
| `REDIS_URL` | `redis://localhost:6379` | Connected but **not actively used** — reserved for future |
| `PORT` | `8080` | |
| `MAX_CLIENTS` | `100` | Connection cap enforced at accept |
| `SNAPSHOT_CACHE_MS` | `500` | Snapshot JSON cached to avoid re-marshaling |
| `BROADCAST_BATCH_MS` | `50` | Delta batching window |
| `JWT_SECRET` | `""` | Not implemented yet — WebSocket accepts all connections |
| `LOG_LEVEL` | `info` | zerolog levels: debug/info/warn/error |
| `EVICTION_TTL_S` | `120` | Seconds since last update before an entry is evicted |
| `EVICTION_INTERVAL_S` | `3` | Eviction sweep cadence in seconds (shared by both hubs) |
| `VALUE_EVICTION_TTL_S` | `120` | Seconds before an unseen value bet is evicted (prematch-only, no live variant) |

## Concurrency

- One `runNATSBridge` goroutine drains the NATS subscription channel (buffer: 256), batches patch ops, and broadcasts.
- One `writePump` goroutine per connected client (per-client send channel buffer: 64).
- Two `sync.RWMutex` on `Hub`: one for the client map (`mu`), one for the odds state (`stateMu`). Keep locks short.
- `connCount` and `msgSent` are `atomic.Int64` — no lock needed.

## Production Image

Uses `FROM scratch` — no shell, no OS. Only the stripped binary + TLS certs are present. Docker exec into this container is not possible.

## Known Gaps / TODOs

- JWT auth on `/ws` is stubbed (`InsecureSkipVerify: true` in accept options)
- Redis client is initialized and pinged but never read/written
