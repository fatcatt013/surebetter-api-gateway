# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Go WebSocket gateway. Subscribes to `arb.opportunities` on NATS JetStream, maintains in-memory state of all current arbitrage opportunities, and pushes delta patches to browser clients via WebSocket.

## Structure

The hubs, WebSocket handling and routes live in `cmd/gateway/` (`main.go`, `auth.go`: who may open a socket, `conns.go`: open sockets per principal, `bot.go`: bot agents' presence, `GET /api/me/bot` and the `/api/me/bot*` routes, `bot_sched.go`: the order scheduler (checks, window pick and reservation, waiting, expiry, events, pause), `bot_day.go`: the Prague day and bookmaker labels, `bot_agents.go`: agent status and the Fleet list, `bot_logs.go`: agent logs, `bot_shots.go`: screenshots, `bot_prune.go`: 7-day retention, `bot_flows.go`: the agents' Flows, `bot_recordings.go`: each agent's mouse and typing recordings, `bot_settings.go`: mode, pause and limits, `bot_orders.go`: the Orders list and cancel, `bot_commands.go`: window commands and Tries, `bot_limits.go`: each agent's limit index (rule 3) and the legs' `limit_level`, `bot_alerts*.go`: bot alerts (Phase 4b: the engine in `botHub.alerts`, window / agent / order triggers, `/api/me/bot/alerts*` and `/alert-settings`), `bot_logins.go`, `bot_logins_cipher.go` and `bot_alerts_logins.go`: bot auto-login (each window's bookmaker login sealed with `BOT_LOGINS_KEY`, the app's and the agent's routes behind `loginGate` (503 without a key, 403 `https_required` unless secure), the reveal cap, results, the login alerts, key rotation at start), `telegram_alerts.go` and `telegram_me.go`: alerts to each user's linked private Telegram chat and `/api/me/telegram*`, `stats.go`: the Statistics API (every signed-in user) over the Telegram sent-arb rows: filters, Prague buckets, KPIs, histograms, pairs, paging and the CSV). `internal/accounts` holds accounts (SQLite: users, invites, sessions, reset codes, extension codes, per-user state, bot agents with their Flows and limit index (migration 8), bot logs, bot settings (with the odds-drop mode, migration 10), bot orders (with their odds-drop mode and minimum profit, migration 10), the agents' recordings and the Telegram bot's sent-arb statistics, bot alerts with their settings and each user's Telegram link (migration 11), each window's bookmaker login (`bot_logins`, migration 12: encrypted, deleted on code revoke and user disable)) and serves `/api/auth`, `/api/me`, `/api/admin`. `internal/telegram` is the Telegram bot (arbs to the group chat; `link.go`: the alert linker reading `getUpdates`); `stats.go` there records every posted arb (one row per message sent, closed with `ended_at` when the arb is gone) through `cmd/gateway/telegram.go`'s `telegramStatsSink` into `telegram_sent_arbs` / `telegram_sent_legs` (migration 7; see `../surebetter-common/docs/telegram-stats.md`).

Key types in that file:
- `Config` — env vars loaded at startup
- `StateHub[T]` — central state of one channel: client map + item state + snapshot cache, parameterized by a `hubSpec` (key func, isLive func — eviction class, removed func, patch path prefix). `Hub` = `StateHub[ArbOpportunity]` (`/ws`, `arb.opportunities`, `/arb/`); `ValueHub` = `StateHub[ValueBet]` (`/ws/value`, `value.opportunities`, `/value/`)
- `ValueBet` — decodes only `id`/`event_id`/`sport`/`is_live`/`removed`; marshals the engine's original JSON verbatim. `isLive()` = `is_live`, fallback: `event_id` sport segment ends in `-live` (`sport` is bare for value bets)
- `Client` — single WebSocket connection with a send channel
- `ArbOpportunity` / `StakeAllocation` / `AltQuote` — mirrors the Rust arb-engine structs (a field missing here is dropped on the re-marshal to clients). `StakeAllocation.Alternatives` (the other books' prices for the leg's outcome, `omitempty` like the engine) is relayed verbatim; every publish is a whole-value `replace` patch, so a change in alternatives alone reaches the clients
- Telegram bot preset with bookmakers: `telegram.ForBooks` / `Repick` (`internal/telegram/filter.go`, `repick.go`) re-pick each outcome's best price among the stake and its alternatives at the preset's books and recompute `profit_pct` / `stake_pct` before every filter step (`Select`), so a post, its edits and its stats row show only the preset's books. No allowed price for an outcome, or a re-picked profit ≤ 0: not posted, and a posted one goes GONE (`end_reason` `removed`). The Statistics API (`stats.go`) is unchanged: it reads the stored rows, which already hold the re-picked legs
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
| `GET /api/stats/arbs`, `GET /api/stats/arbs.csv` | The Statistics page (any signed-in session; the bot setup stays admin only): the Telegram sent-arb rows filtered and aggregated — see `../surebetter-common/docs/api-reference.md` → Statistics |
| `/api/me/bot`, `/api/me/bot/{status,orders,events,agents,logs,logs.txt,screenshots,flows,recordings,settings,commands,alerts,alert-settings}`, `/api/me/bot/agents/{id}/{recordings,windows,try,limit-index}` | Bot orders, the Bots controller and bot alerts — see `../surebetter-common/docs/api-reference.md` → Bot orders, Bots controller, Bot alerts |
| `/api/me/bot/logins`, `/api/me/bot/logins/mine`, `/api/me/bot/logins/{windowId}/{reveal,result}`, `/api/me/bot/agents/{id}/windows/{windowId}/login` | Bot auto-login (HTTPS only, 503 without `BOT_LOGINS_KEY`) — see `../surebetter-common/docs/api-reference.md` → Bot logins |
| `/api/me/telegram`, `/api/me/telegram/{link,test}` | The user's private Telegram chat for bot alerts — see `../surebetter-common/docs/api-reference.md` → Bot alerts |
| `GET /health` | `{"status":"ok","service":"api-gateway","version":"dev"}` |
| `GET /metrics` | Prometheus counters: `gateway_ws_clients_connected`, `gateway_messages_sent_total` |

## WebSocket Protocol

1. On connect: full snapshot → `{"type":"snapshot","data":{...}}`
2. On each NATS message: RFC 6902 patch → `{"type":"patch","ops":[{"op":"replace","path":"/arb/{eventID}:{marketType}:{line:.2f}","value":{...}}]}`
3. Keepalive: `{"type":"ping"}` every 20s (Chrome stops an idle extension service worker after 30 s)
4. Bot presence changed: `{"type":"bot"}` to the user's non-bot `/ws` sockets when an agent's online state, books, dry run, busy, version, host name, windows or protocol change, not for CPU/RAM alone (the app re-fetches `GET /api/me/bot`)
5. Bot order progress: `{"type":"botEvent","order_id","arb_key","event","bookmaker"?,"reason"?,"step"?,"flow"?}` to the same sockets: the agent's events (`accepted`, `leg_ready`, `leg_step` with `step`, `flow` with `flow`, `placed`, `stopped`, `outcome_unknown`, `cashed_out`) and the gateway's own (`waiting` with `reason`, `sent`, and the final ones it decides). A `leg_step` with step `released` (rule 5) also frees that leg's window at once (`storedLeg.Released`: `reservedLocked` skips it; the leg is `placed:false`) and retries the waiting orders; for an unknown order or leg it is ignored (204)
6. Bot alert: `{"type":"alert","alert":{id,kind,severity,agentId,windowId,orderId,title,detail,createdAt,resolvedAt,dismissedAt}}` to the same sockets when an alert whose kind's `app` switch is on is raised, resolved or dismissed

`/ws?cid=<id>` tags a socket with a client id (max 64 chars, `[A-Za-z0-9_-]`); a `PUT /api/me/state/{key}` sent with the same `X-Client-Id` header skips that socket but notifies the user's other sockets (`{"type":"state","key":...}`). `/ws?feed=0` (arb `/ws` only) gets no snapshot or patches, just `state` messages and pings.

`/ws?ticket=…&feed=0&bot=1` is the user's bot socket: only a ticket issued to a bot code (kind `bot`) may open it (403 otherwise); feed always off; one per bot code, several codes (agents) of one user may be online (a new socket with the same code closes the older one with 1000). It receives the orders the gateway picked its agent for (`{"type":"order","order":{…,"dry_run","legs":[{…,"outcome_id","fair_odds","limit_level","window_id"}],"market_type","line","is_live","sport","match","rounding","max_leg","rules_on","odds_drop","min_profit_pct"?,"bankroll"?,"max_order"}}`, only when its last status said `proto` ≥ 2; `odds_drop` is the user's `oddsDrop` setting at send time (`strict` or `rearb`), `strict` when the app's order had no `min_profit_pct` or no `bankroll` (both passed on as received, absent when not sent), `max_order` the settings' `maxOrder`; each leg's `limit_level` 0–2 comes from that agent's limit index at send time: the first entry with the leg's bookmaker, the order's sport and its market type, else 0), `{"type":"pause"}` when the user is paused (and, while paused, on a socket's first protocol-2 status), window commands and Tries (`{"type":"command","command":{…}}`, a Try with `"action":"try"` and the `"order"` it would get, only when its last status said `proto` ≥ 3, answered with `POST /api/me/bot/commands`), `{"type":"recordings"}` when the app uploaded a recording for it, `{"type":"logins"}` (only when its last status said `proto` ≥ 5) after the app saved or removed one of its windows' logins or a result made logins failing (it re-reads `GET /api/me/bot/logins/mine`), and never `bot`/`botEvent`. The socket records whether its upgrade request was secure (`Client.secure`: TLS, `X-Forwarded-Proto: https` set by nginx, or `BOT_LOGINS_ALLOW_HTTP=1`); a login is revealed only to an agent whose socket is.

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
| `ACCOUNTS_DB` | `/data/accounts.db` | SQLite file for users, invites, sessions, reset codes, extension codes, per-user state, bot agents (with their Flows and limit index), bot logs, bot settings (with the rules switched on), bot orders (with each order's last flow), the agents' recordings, the bot alerts (kept 7 days) with each user's alert settings and linked Telegram chat, and the Telegram sent-arb statistics (no pruning yet). Bot screenshots go in `bot-screenshots/` in the same directory (kept 7 days). Compose mounts the `surebetter-gateway-data` volume at `/data`. Back it up (see `docs/DEPLOY.md`) |
| `AUTH_REQUIRED` | `true` (compose: `false` until the frontend has login) | `true`: `/ws`, `/ws/value` and both snapshots need a session cookie or an extension code (both sockets also accept a `?ticket=`); `/api/telegram/*` needs an admin session. Only the exact value `false` (trimmed, case-insensitive) turns it off; anything else, typos included, means required. `false`: those stay open; the account API works either way |
| `LOG_LEVEL` | `info` | zerolog levels: debug/info/warn/error |
| `EVICTION_TTL_S` | `120` | Seconds since last update before an entry is evicted |
| `EVICTION_INTERVAL_S` | `3` | Eviction sweep cadence in seconds (shared by both hubs) |
| `VALUE_EVICTION_TTL_S` | `120` | Seconds before an unseen **prematch** value bet is evicted |
| `VALUE_LIVE_EVICTION_TTL_S` | `15` | Seconds before an unseen **live** value bet (`is_live`, fallback `event_id` sport `-live`) is evicted |
| `TELEGRAM_POLL` | `""` | `1`: Telegram bot alerts on (the `getUpdates` linker and the sends to private chats; needs `TELEGRAM_TOKEN`). One reader per token: only the VPS sets it. Off: `GET /api/me/telegram` says `available: false` |
| `APP_PUBLIC_URL` | `""` | The app's address (`https://host`) for the links in Telegram alerts; unset → the alerts go out without a link |
| `BOT_LOGINS_KEY` | `""` | 32 random bytes, standard base64 (`openssl rand -base64 32`): the key of the bot logins (AES-256-GCM). Missing or not 32 bytes → the logins are off (one error line, every login route 503). Never logged; keep `.env` backed up apart from the DB |
| `BOT_LOGINS_KEY_PREVIOUS` | `""` | The key before a rotation: at start every row of that key is sealed again with `BOT_LOGINS_KEY` (one transaction, the count logged). Malformed → ignored (a warn line) |
| `BOT_LOGINS_ALLOW_HTTP` | `""` | `1`: every request and bot socket counts as secure for the logins (development only; set by `docker-compose.override.yml`, never by `docker-compose.yml`). Logged as a warning |

## Concurrency

- One `runNATSBridge` goroutine drains the NATS subscription channel (buffer: 256), batches patch ops, and broadcasts.
- One `writePump` goroutine per connected client (per-client send channel buffer: 64).
- Two `sync.RWMutex` on `Hub`: one for the client map (`mu`), one for the odds state (`stateMu`). Keep locks short.
- `connCount` and `msgSent` are `atomic.Int64` — no lock needed.
- `botHub.mu` guards the agents' presence, the waiting and running orders and the cached settings. A change collects its database writes, `botEvent`s and `{"type":"bot"}` notices in an `effects` and calls `release`, which takes `botHub.dbMu` before it lets go of `mu`: the writes of successive changes reach SQLite in order, and the app messages go out after both locks. Lock order is always `mu` → `dbMu` (the day-limit query takes `dbMu` under `mu` so it sees every earlier write). Settings are loaded into the cache (`loadSettings`) before `mu` is taken; so is each agent's limit index (`loadLimitIndex`, on its status and before a Try; `botHub.limits`, by code id), which a `PUT …/limit-index` replaces under `botHub.limitMu` (held from the database write to the cache update, so the two never disagree). The per-user log and screenshot rate limiters (`minuteLimiter`, `bot_logs.go`) have their own mutexes; `botHub.shotsMu` holds a user directory's size check through the screenshot write. `runPrune` (`bot_prune.go`) deletes logs and screenshots older than 7 days at start and hourly, and runs the scheduler's `tick` every second (expiry, waiting orders). Order, pause and command sends to a bot socket happen under `mu` (non-blocking). A window command's app request waits up to 10 s for the agent's answer on its own channel (`pendingCommand.done`, buffered), never holding `mu`.
- The Telegram notifier never touches SQLite: it hands each statistics record (sent, end, startup sweep) to a `StatsWriter` (`internal/telegram/stats.go`, queue 1024, its own goroutine) without blocking; a full queue drops the record and a failed write is logged, the message goes out either way (`gateway_telegram_stats_dropped_total`, `gateway_telegram_stats_errors_total`).
- An order queued in the microseconds between a bot socket's `writePump` exiting and its `detach` is lost, like on a TCP-dead socket; it ends `outcome_unknown` after 10 minutes (or `agent restarted` on the agent's next boot id).
- Bot alerts live in `botHub.alerts` (guarded by `mu`): grace timers are due times checked by `tick` (`fireAlertsLocked`); alerts go out through `effects` (row write, `{"type":"alert"}`), and Telegram sends run in `effects.later` after both locks (`telegramAlerts.send` reads the link). With `TELEGRAM_TOKEN` and `TELEGRAM_POLL=1`, one shared Telegram outbox carries arb jobs, `JobAlert` and the linker's `JobReply` (gone > delete > test > alert > send > edit > reply; at most 200 alerts wait — the oldest is dropped with `ErrQueueFull` — and 20 replies — the newest is dropped; a reply is tried once); an alert surely not sent (a 429, or `telegram.NetError.NotSent`: no connection to Telegram was made — DNS failure, connection refused, a connect timeout — traced with `httptrace.GotConn`) is re-queued after 2, 5, 15 s by `time.AfterFunc`, never sleeping in the worker; a timeout or a 5xx is never retried. The alert linker (`telegram.Linker`) long-polls `getUpdates` in its own goroutine (one reader per token: only the VPS sets `TELEGRAM_POLL=1`; backoff 5 s doubling to 60 s); a valid `/start <code>` is answered as a `JobAlert`, anything else with at most one `JobReply` per chat per 30 min. A revoked code reaches the engine through `connRegistry.codeRevoked` (set in `main`; `alertsCodeRevoked`), a disabled user through `connRegistry.userClosed` (`alertsUserClosed`: every `CloseUser`, acted on only when `UserDisabled`; the user's agents are forgotten until one connects again). `POST /api/me/telegram/test` is limited to one per user per 10 s (`telegramTestEvery`). The start check (`checkAgentsAfterStart`, from `tick`, 3 min after start, again every 30 s after a read error with one error log per failure streak; disabled users' agents left out) reads the registry outside `mu`.

## Production Image

Uses `FROM scratch` — no shell, no OS. Only the stripped binary + TLS certs are present. Docker exec into this container is not possible.

## Known Gaps / TODOs

- Redis client is initialized and pinged but never read/written
