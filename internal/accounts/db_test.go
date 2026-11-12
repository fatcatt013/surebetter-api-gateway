package accounts

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenDB_CreatesSchemaAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "accounts.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Fatalf("user_version = %d, want %d", v, len(migrations))
	}
	for _, table := range []string{"users", "invites", "reset_codes", "sessions", "extension_codes", "user_state"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d, err=%v)", table, n, err)
		}
	}
	db.Close()

	db, err = OpenDB(path) // reopening must not re-run migrations
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer db.Close()
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d (err %v), want 1", fk, err)
	}
}

func TestMigrate_ExistingCodesBecomeExtensionKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped before code kinds (version 1), with one code in it.
	for _, q := range []string{
		migrations[0],
		`PRAGMA user_version = 1`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO extension_codes (user_id, name, code_hash, hint, created_at) VALUES (1, 'Laptop', 'h', 'ABCD', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%.40s: %v", q, err)
		}
	}
	db.Close()

	db, err = OpenDB(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()
	var kind string
	if err := db.QueryRow(`SELECT kind FROM extension_codes WHERE name = 'Laptop'`).Scan(&kind); err != nil || kind != "extension" {
		t.Fatalf("existing code kind = %q (err %v), want extension", kind, err)
	}
	if _, err := db.Exec(`UPDATE extension_codes SET kind = 'robot'`); err == nil {
		t.Fatal("kind must be extension or bot")
	}
}

func TestMigrate_BotTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with code kinds (version 2), with one bot code in it.
	for _, q := range []string{
		migrations[0],
		migrations[1],
		`PRAGMA user_version = 2`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO extension_codes (id, user_id, name, kind, code_hash, hint, created_at) VALUES (5, 1, 'Home PC', 'bot', 'h', 'ABCD', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%.40s: %v", q, err)
		}
	}
	db.Close()

	db, err = OpenDB(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO bot_agents (code_id, user_id, first_seen_at, last_seen_at) VALUES (5, 1, 10, 10)`); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	var version, host, windows string
	if err := db.QueryRow(`SELECT version, host_name, windows_json FROM bot_agents WHERE code_id = 5`).Scan(&version, &host, &windows); err != nil ||
		version != "" || host != "" || windows != "[]" {
		t.Fatalf("agent defaults: %q %q %q (err %v)", version, host, windows, err)
	}
	if _, err := db.Exec(`INSERT INTO bot_agents (code_id, user_id, first_seen_at, last_seen_at) VALUES (99, 1, 10, 10)`); err == nil {
		t.Fatal("an agent row needs an existing code")
	}
	if _, err := db.Exec(`INSERT INTO bot_logs (user_id, code_id, ts, level, msg) VALUES (1, 5, 10, 'warn', 'odds dropped')`); err != nil {
		t.Fatalf("insert log: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO bot_logs (user_id, code_id, ts, level, msg) VALUES (1, 5, 10, 'debug', 'x')`); err == nil {
		t.Fatal("level must be info, warn or error")
	}
	for _, index := range []string{"bot_logs_user_id", "bot_logs_ts"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s missing (n=%d, err=%v)", index, n, err)
		}
	}
}
