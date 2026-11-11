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
