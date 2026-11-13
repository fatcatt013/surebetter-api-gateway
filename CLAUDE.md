# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Go WebSocket gateway. Subscribes to `arb.opportunities` on NATS JetStream, maintains in-memory state of all current arbitrage opportunities, and pushes delta patches to browser clients via WebSocket.

## Structure

The hubs, WebSocket handling and routes live in `cmd/gateway/` (`main.go`, `auth.go`: who may open a socket, `conns.go`: open sockets per principal, `bot.go`: bot agents' presence, `GET /api/me/bot` and the `/api/me/bot*` routes, `bot_sched.go`: the order scheduler (checks, window pick and reservation, waiting, expiry, events, pause), `bot_day.go`: the Prague day and bookmaker labels, `bot_agents.go`: agent status and the Fleet list, `bot_logs.go`: agent logs, `bot_shots.go`: screenshots, `bot_prune.go`: 7-day retention, `bot_flows.go`: the agents' Flows, `bot_recordings.go`: each agent's mouse and typing recordings, `bot_settings.go`: mode, pause and limits, `bot_orders.go`: the Orders list and cancel, `bot_commands.go`: window commands). `internal/accounts` holds accounts (SQLite: users, invites, sessions, reset codes, extension codes, per-user state, bot agents with their Flows, bot logs, bot settings, bot orders and the agents' recordings) and serves `/api/auth`, `/api/me`, `/api/admin`. `internal/telegram` is the Telegram bot.

Key types in that file:
- `Config` — env vars loaded at startup
- `StateHub[T]` — central state of one channel: client map + item state + snapshot cache, parameterized by a `hubSpec` (key func, isLive func — eviction class, removed func, patch path prefix). `Hub` = `StateHub[ArbOpportunity]` (`/ws`, `arb.opportunities`, `/arb/`); `ValueHub` = `StateHub[ValueBet]` (`/ws/value`, `value.opportunities`, `/value/`)
- `ValueBet` — decodes only `id`/`event_id`/`sport`/`is_live`/`removed`; marshals the engine's original JSON verbatim. `isLive()` = `is_live`, fallback: `event_id` sport segment ends in `-live` (`sport` is bare for value bets)
- `Client` — single WebSocket connection with a send channel
- `ArbOpportunity` / `StakeAllocation` — mirrors the Rust arb-engine structs
- `WSMessage` / `PatchOp` — WebSocket protocol types (RFC 6902 JSON Patch)
- `botHub` (`bot.go`) — per user, the bot agents online (one socket per bot code: last status, windows, idle-since per window), the waiting and running orders (`botOrder`, `bot_sched.go`; reservations are derived from the running orders) and the cached settings, in memory; the agent registry, logs, settings and order history are in the accounts DB, screenshots in `bot-screenshots/` next to it

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
| `/api/auth/*`, `/api/me/*`, `/api/admin/*` | Accounts API — see `../surebetter-common/docs/api-reference.md` → Accounts |
| `/api/me/bot`, `/api/me/bot/{status,orders,events,agents,logs,logs.txt,screenshots,flows,recordings,settings,commands}`, `/api/me/bot/agents/{id}/{recordings,windows,try}` | Bot orders and the Bots controller — see `../surebetter-common/docs/api-reference.md` → Bot orders, Bots controller |
| `GET /health` | `{"status":"ok","service":"api-gateway","version":"dev"}` |
| `GET /metrics` | Prometheus counters: `gateway_ws_clients_connected`, `gateway_messages_sent_total` |

## WebSocket Protocol

1. On connect: full snapshot → `{"type":"snapshot","data":{...}}`
2. On each NATS message: RFC 6902 patch → `{"type":"patch","ops":[{"op":"replace","path":"/arb/{eventID}:{marketType}:{line:.2f}","value":{...}}]}`
3. Keepalive: `{"type":"ping"}` every 20s (Chrome stops an idle extension service worker after 30 s)
4. Bot presence changed: `{"type":"bot"}` to the user's non-bot `/ws` sockets when an agent's online state, books, dry run, busy, version, host name, windows or protocol change, not for CPU/RAM alone (the app re-fetches `GET /api/me/bot`)
5. Bot order progress: `{"type":"botEvent","order_id","arb_key","event","bookmaker"?,"reason"?,"step"?,"flow"?}` to the same sockets: the agent's events (`accepted`, `leg_ready`, `leg_step` with `step`, `flow` with `flow`, `placed`, `stopped`, `outcome_unknown`, `cashed_out`) and the gateway's own (`waiting` with `reason`, `sent`, and the final ones it decides)

`/ws?cid=<id>` tags a socket with a client id (max 64 chars, `[A-Za-z0-9_-]`); a `PUT /api/me/state/{key}` sent with the same `X-Client-Id` header skips that socket but notifies the user's other sockets (`{"type":"state","key":...}`). `/ws?feed=0` (arb `/ws` only) gets no snapshot or patches, just `state` messages and pings.

`/ws?ticket=…&feed=0&bot=1` is the user's bot socket: only a ticket issued to a bot code (kind `bot`) may open it (403 otherwise); feed always off; one per bot code, several codes (agents) of one user may be online (a new socket with the same code closes the older one with 1000). It receives the orders the gateway picked its agent for (`{"type":"order","order":{…,"dry_run","legs":[{…,"outcome_id","fair_odds","window_id"}],"market_type","line","is_live","sport","match","rounding","max_leg","rules_on"}}`, only when its last status said `proto` ≥ 2), `{"type":"pause"}` when the user is paused (and, while paused, on a socket's first protocol-2 status), window commands and Tries (`{"type":"command","command":{…}}`, a Try with `"action":"try"` and the `"order"` it would get, only when its last status said `proto` ≥ 3, answered with `POST /api/me/bot/commands`), `{"type":"recordings"}` when the app uploaded a recording for it, and never `bot`/`botEvent`.

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
| `JWT_SECRET` | `""` | Unused by the gateway (accounts use opaque session tokens, not JWTs); compose still requires it |
| `ACCOUNTS_DB` | `/data/accounts.db` | SQLite file for users, invites, sessions, reset codes, extension codes, per-user state, bot agents (with their Flows), bot logs, bot settings (with the rules switched on), bot orders (with each order's last flow) and the agents' recordings. Bot screenshots go in `bot-screenshots/` in the same directory (kept 7 days). Compose mounts the `surebetter-gateway-data` volume at `/data`. Back it up (see `docs/DEPLOY.md`) |
| `AUTH_REQUIRED` | `true` (compose: `false` until the frontend has login) | `true`: `/ws`, `/ws/value` and both snapshots need a session cookie or an extension code (both sockets also accept a `?ticket=`); `/api/telegram/*` needs an admin session. Only the exact value `false` (trimmed, case-insensitive) turns it off; anything else, typos included, means required. `false`: those stay open; the account API works either way |
| `LOG_LEVEL` | `info` | zerolog levels: debug/info/warn/error |
| `EVICTION_TTL_S` | `120` | Seconds since last update before an entry is evicted |
| `EVICTION_INTERVAL_S` | `3` | Eviction sweep cadence in seconds (shared by both hubs) |
| `VALUE_EVICTION_TTL_S` | `120` | Seconds before an unseen **prematch** value bet is evicted |
| `VALUE_LIVE_EVICTION_TTL_S` | `15` | Seconds before an unseen **live** value bet (`is_live`, fallback `event_id` sport `-live`) is evicted |

## Concurrency

- One `runNATSBridge` goroutine drains the NATS subscription channel (buffer: 256), batches patch ops, and broadcasts.
- One `writePump` goroutine per connected client (per-client send channel buffer: 64).
- Two `sync.RWMutex` on `Hub`: one for the client map (`mu`), one for the odds state (`stateMu`). Keep locks short.
- `connCount` and `msgSent` are `atomic.Int64` — no lock needed.
- `botHub.mu` guards the agents' presence, the waiting and running orders and the cached settings. A change collects its database writes, `botEvent`s and `{"type":"bot"}` notices in an `effects` and calls `release`, which takes `botHub.dbMu` before it lets go of `mu`: the writes of successive changes reach SQLite in order, and the app messages go out after both locks. Lock order is always `mu` → `dbMu` (the day-limit query takes `dbMu` under `mu` so it sees every earlier write). Settings are loaded into the cache (`loadSettings`) before `mu` is taken. The per-user log and screenshot rate limiters (`minuteLimiter`, `bot_logs.go`) have their own mutexes; `botHub.shotsMu` holds a user directory's size check through the screenshot write. `runPrune` (`bot_prune.go`) deletes logs and screenshots older than 7 days at start and hourly, and runs the scheduler's `tick` every second (expiry, waiting orders). Order, pause and command sends to a bot socket happen under `mu` (non-blocking). A window command's app request waits up to 10 s for the agent's answer on its own channel (`pendingCommand.done`, buffered), never holding `mu`.
- An order queued in the microseconds between a bot socket's `writePump` exiting and its `detach` is lost, like on a TCP-dead socket; it ends `outcome_unknown` after 10 minutes (or `agent restarted` on the agent's next boot id).

## Production Image

Uses `FROM scratch` — no shell, no OS. Only the stripped binary + TLS certs are present. Docker exec into this container is not possible.

## Known Gaps / TODOs

- Redis client is initialized and pinged but never read/written
