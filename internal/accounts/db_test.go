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
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d (err %v), want %d", version, err, len(migrations))
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

func TestMigrate_AgentLimitIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with Phase 3 (version 7) and one agent with its status and Flows.
	steps := append(append([]string{}, migrations[:7]...), `PRAGMA user_version = 7`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO extension_codes (id, user_id, name, kind, code_hash, hint, created_at) VALUES (5, 1, 'Home PC', 'bot', 'h', 'ABCD', 0)`,
		`INSERT INTO bot_agents (code_id, user_id, version, host_name, windows_json, first_seen_at, last_seen_at, flows_json, flows_at)
			VALUES (5, 1, '1.2.3', 'home-pc', '[{"id":"betano-1"}]', 10, 20, '{"rules":[]}', 30)`)
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
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d (err %v), want %d", version, err, len(migrations))
	}
	// The agent row is kept and gains limit_index_json '[]'.
	var row string
	if err := db.QueryRow(`SELECT code_id || '/' || user_id || '/' || version || '/' || host_name || '/' || windows_json || '/' || first_seen_at || '/' ||
		last_seen_at || '/' || flows_json || '/' || flows_at || '/' || limit_index_json FROM bot_agents`).Scan(&row); err != nil ||
		row != `5/1/1.2.3/home-pc/[{"id":"betano-1"}]/10/20/{"rules":[]}/30/[]` {
		t.Fatalf("agent after the migration: %q (err %v)", row, err)
	}
}

func TestMigrate_HumanMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with Phase 3b (version 8), one agent with a limit index and one ended order.
	steps := append(append([]string{}, migrations[:8]...), `PRAGMA user_version = 8`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO extension_codes (id, user_id, name, kind, code_hash, hint, created_at) VALUES (5, 1, 'Home PC', 'bot', 'h', 'ABCD', 0)`,
		`INSERT INTO bot_agents (code_id, user_id, version, host_name, windows_json, first_seen_at, last_seen_at, limit_index_json)
			VALUES (5, 1, '1.2.3', 'home-pc', '[]', 10, 20, '[{"bookmaker":"betano"}]')`,
		`INSERT INTO bot_orders (id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json, created_at, sent_at, ended_at)
			VALUES ('00000000000000aa', 1, 'k', 5, 'outcome_unknown', '', 0, 10, '{}', '[]', 1, 2, 3)`)
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
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d (err %v), want %d", version, err, len(migrations))
	}
	var agent string
	if err := db.QueryRow(`SELECT code_id || '/' || limit_index_json || '/' || routine_json || '/' || human_mode FROM bot_agents`).Scan(&agent); err != nil ||
		agent != `5/[{"bookmaker":"betano"}]//0` {
		t.Fatalf("agent after the migration: %q (err %v)", agent, err)
	}
	var order string
	if err := db.QueryRow(`SELECT id || '/' || state || '/' || ended_at || '/' || checked_at FROM bot_orders`).Scan(&order); err != nil ||
		order != `00000000000000aa/outcome_unknown/3/0` {
		t.Fatalf("order after the migration: %q (err %v)", order, err)
	}
}

func TestMigrate_OddsDrop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with human mode Phase A (version 9), one user's settings and one ended order.
	steps := append(append([]string{}, migrations[:9]...), `PRAGMA user_version = 9`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO bot_settings (user_id, dry_run, max_leg, updated_at) VALUES (1, 0, 3000, 5)`,
		`INSERT INTO bot_orders (id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json, created_at, sent_at, ended_at)
			VALUES ('00000000000000aa', 1, 'k', 0, 'placed', '', 0, 10, '{}', '[]', 1, 2, 3)`)
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
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d (err %v), want %d", version, err, len(migrations))
	}
	var settings string
	if err := db.QueryRow(`SELECT dry_run || '/' || max_leg || '/' || odds_drop FROM bot_settings`).Scan(&settings); err != nil ||
		settings != `0/3000/strict` {
		t.Fatalf("settings after the migration: %q (err %v)", settings, err)
	}
	var order string
	if err := db.QueryRow(`SELECT id || '/' || state || '/' || odds_drop || '/' || min_profit_pct FROM bot_orders`).Scan(&order); err != nil ||
		order != `00000000000000aa/placed/strict/0.0` {
		t.Fatalf("order after the migration: %q (err %v)", order, err)
	}
}

func TestMigrate_BotAlerts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with odds drop (version 10) and one user.
	steps := append(append([]string{}, migrations[:10]...), `PRAGMA user_version = 10`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`)
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
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d (err %v), want %d", version, err, len(migrations))
	}
	for _, q := range []string{
		`INSERT INTO bot_alert_settings (user_id, json, updated_at) VALUES (1, '{}', 5)`,
		`INSERT INTO bot_alerts (id, user_id, kind, severity, title, created_at) VALUES ('a', 1, 'window', 'problem', 'Logged out', 5)`,
		`INSERT INTO telegram_links (user_id, chat_id, linked_at) VALUES (1, 555, 5)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%.60s: %v", q, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO bot_alerts (id, user_id, kind, severity, title, created_at) VALUES ('b', 1, 'weather', 'problem', 'x', 5)`,
		`INSERT INTO bot_alerts (id, user_id, kind, severity, title, created_at) VALUES ('c', 1, 'window', 'loud', 'x', 5)`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Fatalf("accepted: %s", q)
		}
	}
	var defaults string
	if err := db.QueryRow(`SELECT agent_code_id || '/' || window_id || '/' || order_id || '/' || detail || '/' || resolved_at || '/' || dismissed_at
		FROM bot_alerts`).Scan(&defaults); err != nil || defaults != "0////0/0" {
		t.Fatalf("alert defaults: %q (err %v)", defaults, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'bot_alerts_user_created'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("index bot_alerts_user_created: %d (err %v)", n, err)
	}
}

func TestMigrate_BotLogins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with bot alerts (version 11), one user and one open alert.
	steps := append(append([]string{}, migrations[:11]...), `PRAGMA user_version = 11`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO bot_alerts (id, user_id, kind, severity, title, created_at) VALUES ('a', 1, 'window', 'problem', 'Logged out', 5)`)
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
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d (err %v), want %d", version, err, len(migrations))
	}
	var alerts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bot_alerts`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("alerts kept: %d (err %v)", alerts, err)
	}
	if _, err := db.Exec(`INSERT INTO bot_logins (user_id, code_id, window_id, bookmaker, secret, key_id, user_tag, updated_at)
		VALUES (1, 3, 'tipsport-1', 'tipsport', x'00', 'k', 't', 5)`); err != nil {
		t.Fatal(err)
	}
	var defaults string
	if err := db.QueryRow(`SELECT last_ok_at || '/' || last_result || '/' || last_at || '/' || failing || '/' || reveals_day || '/' || reveals
		FROM bot_logins`).Scan(&defaults); err != nil || defaults != "0//0///0" {
		t.Fatalf("defaults: %q (err %v)", defaults, err)
	}
	for _, q := range []string{
		`INSERT INTO bot_logins (user_id, code_id, window_id, bookmaker, secret, key_id, user_tag, updated_at) VALUES (1, 3, 'tipsport-1', 'tipsport', x'00', 'k', 't', 5)`,
		`INSERT INTO bot_logins (user_id, code_id, window_id, bookmaker, secret, key_id, user_tag, updated_at) VALUES (9, 3, 'tipsport-2', 'tipsport', x'00', 'k', 't', 5)`,
		`UPDATE bot_logins SET last_result = 'maybe'`,
		`UPDATE bot_logins SET failing = 'code'`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Fatalf("accepted: %s", q)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'bot_logins_user'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("index bot_logins_user: %d (err %v)", n, err)
	}
}

func TestMigrate_BotAutoMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with bot auto-login (version 12), one user, their settings and one order.
	steps := append(append([]string{}, migrations[:12]...), `PRAGMA user_version = 12`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO bot_settings (user_id, dry_run, paused, paused_reason, max_order, max_leg, per_day, orders_at_once, pause_on_unknown, updated_at)
			VALUES (1, 0, 0, '', 1000, 500, 5000, 1, 1, 5)`,
		`INSERT INTO bot_orders (id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json, created_at)
			VALUES ('o1', 1, 'k', 0, 'placed', '', 0, 200, '{}', '[]', 7)`)
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
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d (err %v), want %d", version, err, len(migrations))
	}
	var settings, order string
	if err := db.QueryRow(`SELECT max_leg || '/' || auto_json || '/' || auto_paused_reason || '/' || auto_paused_orders FROM bot_settings`).
		Scan(&settings); err != nil || settings != "500///[]" {
		t.Fatalf("settings after the migration: %q (err %v)", settings, err)
	}
	if err := db.QueryRow(`SELECT stake || '/' || source || '/' || event_id FROM bot_orders`).Scan(&order); err != nil || order != "200/app/" {
		t.Fatalf("order after the migration: %q (err %v)", order, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'bot_orders_user_event'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("index bot_orders_user_event: %d (err %v)", n, err)
	}
}

func TestMigrate_ExtensionCodesViewDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	// The schema as shipped with bot automatic mode (version 13): one user, a revoked and a live bot code with an agent,
	// a recording, a login and an order.
	steps := append(append([]string{}, migrations[:13]...), `PRAGMA user_version = 13`,
		`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'u@example.com', 'x', 'user', 0)`,
		`INSERT INTO extension_codes (id, user_id, name, code_hash, hint, created_at, revoked_at, kind) VALUES (1, 1, 'Old', 'h1', 'AAAA', 1, 2, 'bot')`,
		`INSERT INTO extension_codes (id, user_id, name, code_hash, hint, created_at, kind) VALUES (2, 1, 'PC', 'h2', 'BBBB', 3, 'bot')`,
		`INSERT INTO bot_agents (code_id, user_id, first_seen_at, last_seen_at) VALUES (2, 1, 4, 5)`,
		`INSERT INTO bot_recordings (code_id, user_id, kind, body_json, count, updated_at) VALUES (2, 1, 'mouse', '[]', 0, 6)`,
		`INSERT INTO bot_logins (user_id, code_id, window_id, bookmaker, secret, key_id, user_tag, updated_at) VALUES (1, 2, 'tipsport-1', 'tipsport', x'00', 'k', 't', 7)`,
		`INSERT INTO bot_orders (id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json, created_at)
			VALUES ('o1', 1, 'k', 2, 'placed', '', 0, 200, '{}', '[]', 8)`)
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
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 14 || version != len(migrations) {
		t.Fatalf("user_version = %d (err %v), want 14", version, err)
	}
	var codes string
	if err := db.QueryRow(`SELECT group_concat(id || ':' || name || ':' || COALESCE(revoked_at, '-') || ':' || (code_enc IS NULL), ',')
		FROM (SELECT * FROM extension_codes ORDER BY id)`).Scan(&codes); err != nil || codes != "1:Old:2:1,2:PC:-:1" {
		t.Fatalf("codes after the migration: %q (err %v)", codes, err)
	}
	var floor int64
	if err := db.QueryRow(`SELECT max_deleted FROM extension_code_ids WHERE id = 1`).Scan(&floor); err != nil || floor != 0 {
		t.Fatalf("floor: %d (err %v)", floor, err)
	}
	var n int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM bot_agents) + (SELECT COUNT(*) FROM bot_recordings) + (SELECT COUNT(*) FROM bot_logins)
		+ (SELECT COUNT(*) FROM bot_orders)`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("bot rows after the migration: %d (err %v)", n, err)
	}
	if _, err := db.Exec(`INSERT INTO extension_code_ids (id, max_deleted) VALUES (2, 0)`); err == nil {
		t.Fatal("a second floor row was accepted")
	}
}
