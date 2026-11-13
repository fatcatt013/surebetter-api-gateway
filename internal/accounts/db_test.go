package accounts

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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

func TestMigrate_BotSettingsOrdersRecordingsAndFlows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with Phase 1 of the bots controller (version 4), with one agent in it.
	for _, q := range []string{
		migrations[0], migrations[1], migrations[2], migrations[3],
		`PRAGMA user_version = 4`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO extension_codes (id, user_id, name, kind, code_hash, hint, created_at) VALUES (5, 1, 'Home PC', 'bot', 'h', 'ABCD', 0)`,
		`INSERT INTO bot_agents (code_id, user_id, first_seen_at, last_seen_at) VALUES (5, 1, 10, 10)`,
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
	var flows string
	var flowsAt int64
	if err := db.QueryRow(`SELECT flows_json, flows_at FROM bot_agents WHERE code_id = 5`).Scan(&flows, &flowsAt); err != nil || flows != "" || flowsAt != 0 {
		t.Fatalf("existing agent flows: %q %d (err %v)", flows, flowsAt, err)
	}
	if _, err := db.Exec(`INSERT INTO bot_settings (user_id, updated_at) VALUES (1, 10)`); err != nil {
		t.Fatalf("insert settings: %v", err)
	}
	var dryRun, paused, maxOrder, maxLeg, perDay, atOnce, onUnknown int
	var reason string
	if err := db.QueryRow(`SELECT dry_run, paused, paused_reason, max_order, max_leg, per_day, orders_at_once, pause_on_unknown
		FROM bot_settings WHERE user_id = 1`).Scan(&dryRun, &paused, &reason, &maxOrder, &maxLeg, &perDay, &atOnce, &onUnknown); err != nil ||
		dryRun != 1 || paused != 0 || reason != "" || maxOrder != 0 || maxLeg != 0 || perDay != 0 || atOnce != 1 || onUnknown != 1 {
		t.Fatalf("settings defaults: %d %d %q %d %d %d %d %d (err %v)", dryRun, paused, reason, maxOrder, maxLeg, perDay, atOnce, onUnknown, err)
	}
	if _, err := db.Exec(`INSERT INTO bot_orders (id, user_id, arb_key, state, dry_run, stake, legs_json, created_at)
		VALUES ('0123456789abcdef', 1, 'k', 'waiting', 1, 202, '[]', 10)`); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	var codeID, sentAt, endedAt int64
	var info string
	if err := db.QueryRow(`SELECT code_id, reason, info_json, sent_at, ended_at FROM bot_orders WHERE id = '0123456789abcdef'`).
		Scan(&codeID, &reason, &info, &sentAt, &endedAt); err != nil || codeID != 0 || reason != "" || info != "{}" || sentAt != 0 || endedAt != 0 {
		t.Fatalf("order defaults: %d %q %q %d %d (err %v)", codeID, reason, info, sentAt, endedAt, err)
	}
	if _, err := db.Exec(`UPDATE bot_orders SET state = 'lost'`); err == nil {
		t.Fatal("state must be waiting, running, placed, stopped or outcome_unknown")
	}
	if _, err := db.Exec(`INSERT INTO bot_recordings (code_id, user_id, kind, body_json, count, updated_at) VALUES (5, 1, 'mouse', '{}', 1, 10)`); err != nil {
		t.Fatalf("insert recording: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO bot_recordings (code_id, user_id, kind, body_json, count, updated_at) VALUES (5, 1, 'mouse', '{}', 2, 11)`); err == nil {
		t.Fatal("one recording per agent and kind")
	}
	if _, err := db.Exec(`INSERT INTO bot_recordings (code_id, user_id, kind, body_json, count, updated_at) VALUES (5, 1, 'voice', '{}', 1, 10)`); err == nil {
		t.Fatal("kind must be mouse or typing")
	}
	if _, err := db.Exec(`INSERT INTO bot_recordings (code_id, user_id, kind, body_json, count, updated_at) VALUES (99, 1, 'typing', '{}', 1, 10)`); err == nil {
		t.Fatal("a recording needs an existing code")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'bot_orders_user_created'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("index bot_orders_user_created missing (n=%d, err=%v)", n, err)
	}
}

func TestMigrate_RulesOnFlowAndCashedOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with Phase 2 of the bots controller (version 5): settings and one order in
	// every state, each with its own values.
	steps := []string{migrations[0], migrations[1], migrations[2], migrations[3], migrations[4], `PRAGMA user_version = 5`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO extension_codes (id, user_id, name, kind, code_hash, hint, created_at) VALUES (5, 1, 'Home PC', 'bot', 'h', 'ABCD', 0)`,
		`INSERT INTO bot_settings (user_id, dry_run, paused, paused_reason, max_order, max_leg, per_day, orders_at_once, pause_on_unknown, updated_at)
			VALUES (1, 0, 1, 'Paused', 5000, 3000, 20000, 3, 0, 77)`,
	}
	for i, state := range []string{"waiting", "running", "placed", "stopped", "outcome_unknown"} {
		steps = append(steps, fmt.Sprintf(`INSERT INTO bot_orders (id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json,
			created_at, sent_at, ended_at) VALUES ('%016x', 1, 'k%d', 5, '%s', 'r%d', %d, %d, '{"match":"M%d"}', '[{"bookmaker":"b%d"}]', %d, %d, %d)`,
			i+1, i, state, i, i%2, 100+i, i, i, 1000+i, 2000+i, 3000+i))
	}
	// A live stopped order: nothing was bet on it, so migration 6 sets its stake to 0 (the day sum
	// counts stopped orders from Phase 3 on). The dry-run stopped order above keeps its stake.
	steps = append(steps, `INSERT INTO bot_orders (id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json,
		created_at, sent_at, ended_at) VALUES ('0000000000000006', 1, 'k5', 5, 'stopped', 'r5', 0, 105, '{"match":"M5"}', '[{"bookmaker":"b5"}]', 1005, 2005, 3005)`)
	for _, q := range steps {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%.60s: %v", q, err)
		}
	}
	db.Close()

	db, err = OpenDB(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 6 {
		t.Fatalf("user_version = %d (err %v), want 6", version, err)
	}
	// Settings: the row is kept and gains rules_on '[]'.
	var settings string
	if err := db.QueryRow(`SELECT dry_run || '/' || paused || '/' || paused_reason || '/' || max_order || '/' || max_leg || '/' || per_day || '/' ||
		orders_at_once || '/' || pause_on_unknown || '/' || updated_at || '/' || rules_on FROM bot_settings WHERE user_id = 1`).Scan(&settings); err != nil ||
		settings != "0/1/Paused/5000/3000/20000/3/0/77/[]" {
		t.Fatalf("settings after the migration: %q (err %v)", settings, err)
	}
	// Orders: every row and column is kept; flow_json starts empty.
	rows, err := db.Query(`SELECT id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json, created_at, sent_at, ended_at, flow_json
		FROM bot_orders ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var id, arbKey, state, reason, info, legs, flow string
		var userID, codeID, dryRun, stake, created, sent, ended int64
		if err := rows.Scan(&id, &userID, &arbKey, &codeID, &state, &reason, &dryRun, &stake, &info, &legs, &created, &sent, &ended, &flow); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %d %s %d %s %s %d %d %s %s %d %d %d %q", id, userID, arbKey, codeID, state, reason, dryRun, stake, info, legs,
			created, sent, ended, flow))
	}
	rows.Close()
	want := []string{
		`0000000000000001 1 k0 5 waiting r0 0 100 {"match":"M0"} [{"bookmaker":"b0"}] 1000 2000 3000 ""`,
		`0000000000000002 1 k1 5 running r1 1 101 {"match":"M1"} [{"bookmaker":"b1"}] 1001 2001 3001 ""`,
		`0000000000000003 1 k2 5 placed r2 0 102 {"match":"M2"} [{"bookmaker":"b2"}] 1002 2002 3002 ""`,
		`0000000000000004 1 k3 5 stopped r3 1 103 {"match":"M3"} [{"bookmaker":"b3"}] 1003 2003 3003 ""`,
		`0000000000000005 1 k4 5 outcome_unknown r4 0 104 {"match":"M4"} [{"bookmaker":"b4"}] 1004 2004 3004 ""`,
		`0000000000000006 1 k5 5 stopped r5 0 0 {"match":"M5"} [{"bookmaker":"b5"}] 1005 2005 3005 ""`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("orders after the migration:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The state CHECK now takes cashed_out and still refuses anything else; the user reference holds.
	if _, err := db.Exec(`UPDATE bot_orders SET state = 'cashed_out' WHERE id = '0000000000000003'`); err != nil {
		t.Fatalf("cashed_out: %v", err)
	}
	if _, err := db.Exec(`UPDATE bot_orders SET state = 'lost' WHERE id = '0000000000000003'`); err == nil {
		t.Fatal("state must be waiting, running, placed, stopped, outcome_unknown or cashed_out")
	}
	if _, err := db.Exec(`INSERT INTO bot_orders (id, user_id, arb_key, state, dry_run, stake, legs_json, created_at)
		VALUES ('00000000000000ff', 99, 'k', 'waiting', 1, 1, '[]', 1)`); err == nil {
		t.Fatal("an order needs an existing user")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'bot_orders_user_created'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("index bot_orders_user_created missing (n=%d, err=%v)", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'bot_orders_%' AND type = 'table'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a leftover rebuild table (n=%d, err=%v)", n, err)
	}
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check: %q (err %v)", check, err)
	}
	fk, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if fk.Next() {
		t.Error("foreign_key_check found a violation")
	}
	fk.Close()
}
