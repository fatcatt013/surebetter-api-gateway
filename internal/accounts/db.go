// Package accounts holds SureBetter's users, invites, sessions, password resets, extension codes
// and per-user state in a SQLite file, and serves /api/auth, /api/me and /api/admin.
package accounts

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go: the image stays FROM scratch)
)

// migrations[i] moves the schema from version i to i+1 (PRAGMA user_version). Append only;
// never edit an entry that has shipped. All times are Unix milliseconds.
var migrations = []string{
	`CREATE TABLE users (
		id            INTEGER PRIMARY KEY,
		email         TEXT    NOT NULL UNIQUE,
		password_hash TEXT    NOT NULL,
		role          TEXT    NOT NULL CHECK (role IN ('admin', 'user')),
		created_at    INTEGER NOT NULL,
		onboarded_at  INTEGER,
		disabled_at   INTEGER,
		last_seen_at  INTEGER
	);
	CREATE TABLE invites (
		id         INTEGER PRIMARY KEY,
		code_hash  TEXT    NOT NULL UNIQUE,
		hint       TEXT    NOT NULL,
		role       TEXT    NOT NULL CHECK (role IN ('admin', 'user')),
		created_by INTEGER REFERENCES users (id),
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		used_by    INTEGER REFERENCES users (id),
		used_at    INTEGER,
		revoked_at INTEGER,
		bootstrap  INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE reset_codes (
		id         INTEGER PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users (id),
		code_hash  TEXT    NOT NULL UNIQUE,
		hint       TEXT    NOT NULL,
		created_by INTEGER NOT NULL REFERENCES users (id),
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		used_at    INTEGER
	);
	CREATE TABLE sessions (
		id           INTEGER PRIMARY KEY,
		token_hash   TEXT    NOT NULL UNIQUE,
		user_id      INTEGER NOT NULL REFERENCES users (id),
		created_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL
	);
	CREATE INDEX sessions_user ON sessions (user_id);
	CREATE TABLE extension_codes (
		id           INTEGER PRIMARY KEY,
		user_id      INTEGER NOT NULL REFERENCES users (id),
		name         TEXT    NOT NULL,
		code_hash    TEXT    NOT NULL UNIQUE,
		hint         TEXT    NOT NULL,
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER,
		revoked_at   INTEGER
	);
	CREATE TABLE user_state (
		user_id    INTEGER NOT NULL REFERENCES users (id),
		key        TEXT    NOT NULL,
		value      TEXT    NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (user_id, key)
	);`,
	// 2: code kinds. A bot code can also open the bot socket (/ws?bot=1) and post bot status and events.
	`ALTER TABLE extension_codes ADD COLUMN kind TEXT NOT NULL DEFAULT 'extension' CHECK (kind IN ('extension', 'bot'));`,
	// 3: bot agents (one per bot code that has connected) and their log lines (kept 7 days).
	`CREATE TABLE bot_agents (
		code_id       INTEGER PRIMARY KEY REFERENCES extension_codes (id),
		user_id       INTEGER NOT NULL REFERENCES users (id),
		version       TEXT    NOT NULL DEFAULT '',
		host_name     TEXT    NOT NULL DEFAULT '',
		windows_json  TEXT    NOT NULL DEFAULT '[]',
		first_seen_at INTEGER NOT NULL,
		last_seen_at  INTEGER NOT NULL
	);
	CREATE TABLE bot_logs (
		id        INTEGER PRIMARY KEY,
		user_id   INTEGER NOT NULL REFERENCES users (id),
		code_id   INTEGER NOT NULL,
		ts        INTEGER NOT NULL,
		level     TEXT    NOT NULL CHECK (level IN ('info', 'warn', 'error')),
		window_id TEXT    NOT NULL DEFAULT '',
		order_id  TEXT    NOT NULL DEFAULT '',
		step      TEXT    NOT NULL DEFAULT '',
		msg       TEXT    NOT NULL,
		detail    TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX bot_logs_user_id ON bot_logs (user_id, id);
	CREATE INDEX bot_logs_ts ON bot_logs (ts);`,
	// 4: the problems filter (warn and error lines) reads only those lines.
	`CREATE INDEX bot_logs_problems ON bot_logs (user_id, id) WHERE level <> 'info';`,
	// 5: bots controller Phase 2: per-user bot settings (mode, pause, limits), the order history, each
	// agent's mouse and typing recordings, and each agent's Flows upload.
	`CREATE TABLE bot_settings (
		user_id          INTEGER PRIMARY KEY REFERENCES users (id),
		dry_run          INTEGER NOT NULL DEFAULT 1,
		paused           INTEGER NOT NULL DEFAULT 0,
		paused_reason    TEXT    NOT NULL DEFAULT '',
		max_order        INTEGER NOT NULL DEFAULT 0,
		max_leg          INTEGER NOT NULL DEFAULT 0,
		per_day          INTEGER NOT NULL DEFAULT 0,
		orders_at_once   INTEGER NOT NULL DEFAULT 1,
		pause_on_unknown INTEGER NOT NULL DEFAULT 1,
		updated_at       INTEGER NOT NULL
	);
	CREATE TABLE bot_orders (
		id          TEXT    PRIMARY KEY,
		user_id     INTEGER NOT NULL REFERENCES users (id),
		arb_key     TEXT    NOT NULL,
		code_id     INTEGER NOT NULL DEFAULT 0,
		state       TEXT    NOT NULL CHECK (state IN ('waiting', 'running', 'placed', 'stopped', 'outcome_unknown')),
		reason      TEXT    NOT NULL DEFAULT '',
		dry_run     INTEGER NOT NULL,
		stake       INTEGER NOT NULL,
		info_json   TEXT    NOT NULL DEFAULT '{}',
		legs_json   TEXT    NOT NULL,
		created_at  INTEGER NOT NULL,
		sent_at     INTEGER NOT NULL DEFAULT 0,
		ended_at    INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX bot_orders_user_created ON bot_orders (user_id, created_at);
	CREATE TABLE bot_recordings (
		code_id    INTEGER NOT NULL REFERENCES extension_codes (id),
		user_id    INTEGER NOT NULL REFERENCES users (id),
		kind       TEXT    NOT NULL CHECK (kind IN ('mouse', 'typing')),
		body_json  TEXT    NOT NULL,
		count      INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (code_id, kind)
	);
	ALTER TABLE bot_agents ADD COLUMN flows_json TEXT NOT NULL DEFAULT '';
	ALTER TABLE bot_agents ADD COLUMN flows_at INTEGER NOT NULL DEFAULT 0;`,
	// 6: bots controller Phase 3: the rules each user switched on, each order's last flow, and the
	// final state cashed_out. SQLite cannot change a CHECK in place, so bot_orders is rebuilt: a copy
	// with the new CHECK and column, every row copied, the old table dropped, the copy renamed and its
	// index created again (nothing references bot_orders, so foreign keys stay as they are). From
	// Phase 3 on a row's stake is its counted stake and the day sum counts stopped orders, so the live
	// stopped rows (nothing was bet on them) get stake 0. It runs in migrate's transaction: it is all
	// or nothing.
	`ALTER TABLE bot_settings ADD COLUMN rules_on TEXT NOT NULL DEFAULT '[]';
	CREATE TABLE bot_orders_v6 (
		id          TEXT    PRIMARY KEY,
		user_id     INTEGER NOT NULL REFERENCES users (id),
		arb_key     TEXT    NOT NULL,
		code_id     INTEGER NOT NULL DEFAULT 0,
		state       TEXT    NOT NULL CHECK (state IN ('waiting', 'running', 'placed', 'stopped', 'outcome_unknown', 'cashed_out')),
		reason      TEXT    NOT NULL DEFAULT '',
		dry_run     INTEGER NOT NULL,
		stake       INTEGER NOT NULL,
		info_json   TEXT    NOT NULL DEFAULT '{}',
		legs_json   TEXT    NOT NULL,
		created_at  INTEGER NOT NULL,
		sent_at     INTEGER NOT NULL DEFAULT 0,
		ended_at    INTEGER NOT NULL DEFAULT 0,
		flow_json   TEXT    NOT NULL DEFAULT ''
	);
	INSERT INTO bot_orders_v6 (id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json, created_at, sent_at, ended_at)
		SELECT id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json, created_at, sent_at, ended_at FROM bot_orders;
	UPDATE bot_orders_v6 SET stake = 0 WHERE state = 'stopped' AND dry_run = 0;
	DROP TABLE bot_orders;
	ALTER TABLE bot_orders_v6 RENAME TO bot_orders;
	CREATE INDEX bot_orders_user_created ON bot_orders (user_id, created_at);`,
	// 7: statistics of the arbs the Telegram bot posts: one row per message sent (edits add none),
	// its legs in their own table so they group by bookmaker. user_id is the admin who last saved
	// the bot config (NULL = unknown); no foreign key, so a stats row never fails on it. ended_at is
	// when the bot learned the arb was gone (NULL while it lives, or when the end is unknown:
	// end_reason 'unknown'). Kept forever for now (no pruning).
	`CREATE TABLE telegram_sent_arbs (
		id                INTEGER PRIMARY KEY,
		sent_at           INTEGER NOT NULL,
		user_id           INTEGER,
		message_id        INTEGER NOT NULL,
		arb_key           TEXT    NOT NULL,
		first_detected_at INTEGER,
		sport             TEXT    NOT NULL,
		is_live           INTEGER NOT NULL,
		market_type       TEXT    NOT NULL,
		line              REAL    NOT NULL DEFAULT 0,
		profit_pct        REAL    NOT NULL,
		shown_profit_pct  REAL    NOT NULL,
		currency          TEXT    NOT NULL,
		total_stake       INTEGER NOT NULL,
		payout            INTEGER NOT NULL,
		profit            INTEGER NOT NULL,
		ended_at          INTEGER,
		end_reason        TEXT CHECK (end_reason IN ('removed', 'evicted', 'redetected', 'absent', 'unknown'))
	);
	CREATE INDEX telegram_sent_arbs_sent ON telegram_sent_arbs (sent_at);
	CREATE INDEX telegram_sent_arbs_user_sent ON telegram_sent_arbs (user_id, sent_at);
	CREATE INDEX telegram_sent_arbs_open ON telegram_sent_arbs (message_id) WHERE end_reason IS NULL;
	CREATE TABLE telegram_sent_legs (
		sent_id    INTEGER NOT NULL REFERENCES telegram_sent_arbs (id) ON DELETE CASCADE,
		idx        INTEGER NOT NULL,
		bookmaker  TEXT    NOT NULL,
		outcome_id TEXT    NOT NULL,
		odds       REAL    NOT NULL,
		stake      INTEGER NOT NULL,
		PRIMARY KEY (sent_id, idx)
	);
	CREATE INDEX telegram_sent_legs_bookmaker ON telegram_sent_legs (bookmaker, sent_id);`,
	// 8: bots controller Phase 3b (rule 3): each agent's limit index, a JSON array of
	// {"bookmaker","sport","marketTypes":[…],"level":0|1|2} set by hand in the Fleet; '[]' = every cell Normal.
	`ALTER TABLE bot_agents ADD COLUMN limit_index_json TEXT NOT NULL DEFAULT '[]';`,
	// 9: bot human mode Phase A: each agent's routine (a JSON object, '' = none) and human-mode switch
	// (off by default), and when the user marked an ended order as checked (0 = not checked).
	`ALTER TABLE bot_agents ADD COLUMN routine_json TEXT NOT NULL DEFAULT '';
	ALTER TABLE bot_agents ADD COLUMN human_mode INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE bot_orders ADD COLUMN checked_at INTEGER NOT NULL DEFAULT 0;`,
	// 10: odds drop (keep a pair while it is still an arb): the user's setting ('strict' = stop the
	// pair on any drop, 'rearb' = size the stakes again while the order's minimum profit holds) and,
	// on each order, the mode it went with and its minimum profit % (0 = none).
	`ALTER TABLE bot_settings ADD COLUMN odds_drop TEXT NOT NULL DEFAULT 'strict';
	ALTER TABLE bot_orders ADD COLUMN odds_drop TEXT NOT NULL DEFAULT 'strict';
	ALTER TABLE bot_orders ADD COLUMN min_profit_pct REAL NOT NULL DEFAULT 0;`,
}

// OpenDB opens the accounts database at path (creating the file and its directory if needed)
// and applies pending migrations.
func OpenDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite has a single writer anyway, and this keeps every statement
	// on a connection that ran the pragmas above.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: set version: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
