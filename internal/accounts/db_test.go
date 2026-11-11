package accounts

import (
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
