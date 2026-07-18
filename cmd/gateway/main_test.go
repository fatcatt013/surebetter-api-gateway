package main

import (
	"encoding/json"
	"testing"
	"time"
)

// ── ArbOpportunity deserialization ────────────────────────────────────────────

const fullOppJSON = `{
	"event_id":    "football:a1b2c3d4e5f6",
	"sport":       "football",
	"market_type": "MATCH_WINNER",
	"line":        0.0,
	"profit_pct":  2.5,
	"detected_at": 1000000,
	"name_home":   "arsenal",
	"name_away":   "chelsea",
	"league":      "premier league",
	"market_name": "zapas",
	"start_time":  1775030400000,
	"stakes": [
		{"bookmaker":"betano","outcome_id":"HOME","decimal_odds":2.10,"stake_pct":0.476},
		{"bookmaker":"fortuna","outcome_id":"AWAY","decimal_odds":3.50,"stake_pct":0.286}
	]
}`

func TestArbOpportunity_AllFieldsDeserialized(t *testing.T) {
	var opp ArbOpportunity
	if err := json.Unmarshal([]byte(fullOppJSON), &opp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if opp.NameHome != "arsenal" {
		t.Errorf("NameHome = %q, want arsenal", opp.NameHome)
	}
	if opp.NameAway != "chelsea" {
		t.Errorf("NameAway = %q, want chelsea", opp.NameAway)
	}
	if opp.Sport != "football" {
		t.Errorf("Sport = %q, want football", opp.Sport)
	}
	if opp.Line != 0.0 {
		t.Errorf("Line = %v, want 0.0", opp.Line)
	}
	if opp.League != "premier league" {
		t.Errorf("League = %q, want premier league", opp.League)
	}
	if opp.MarketName != "zapas" {
		t.Errorf("MarketName = %q, want zapas", opp.MarketName)
	}
	if opp.StartTime != 1775030400000 {
		t.Errorf("StartTime = %d, want 1775030400000", opp.StartTime)
	}
}

// ── oppStateKey ───────────────────────────────────────────────────────────────

func TestOppStateKey_NoLine(t *testing.T) {
	opp := ArbOpportunity{EventID: "football:abc123", MarketType: "MATCH_WINNER", Line: 0.0}
	got := oppStateKey(opp)
	want := "football:abc123:MATCH_WINNER:0.00"
	if got != want {
		t.Errorf("oppStateKey = %q, want %q", got, want)
	}
}

func TestOppStateKey_WithLine(t *testing.T) {
	opp := ArbOpportunity{EventID: "football:abc123", MarketType: "TOTAL_GOALS", Line: 2.5}
	got := oppStateKey(opp)
	want := "football:abc123:TOTAL_GOALS:2.50"
	if got != want {
		t.Errorf("oppStateKey = %q, want %q", got, want)
	}
}

// ── Hub.ApplyOpportunity ──────────────────────────────────────────────────────

func TestHub_ApplyOpportunity_StoresUnderCompositeKey(t *testing.T) {
	hub := NewHub(Config{SnapshotCacheMs: 500})
	var opp ArbOpportunity
	json.Unmarshal([]byte(fullOppJSON), &opp)

	hub.ApplyOpportunity(opp)

	hub.stateMu.RLock()
	defer hub.stateMu.RUnlock()
	key := "football:a1b2c3d4e5f6:MATCH_WINNER:0.00"
	if _, ok := hub.state[key]; !ok {
		t.Errorf("hub.state missing key %q; got keys: %v", key, stateKeys(hub.state))
	}
}

func TestHub_MultipleMarketsPerEvent_BothStored(t *testing.T) {
	hub := NewHub(Config{SnapshotCacheMs: 500})
	opp1 := ArbOpportunity{EventID: "football:abc", MarketType: "MATCH_WINNER", Line: 0.0}
	opp2 := ArbOpportunity{EventID: "football:abc", MarketType: "TOTAL_GOALS", Line: 2.5}

	hub.ApplyOpportunity(opp1)
	hub.ApplyOpportunity(opp2)

	hub.stateMu.RLock()
	defer hub.stateMu.RUnlock()
	if len(hub.state) != 2 {
		t.Errorf("hub.state has %d entries, want 2 — markets for same event should not overwrite each other", len(hub.state))
	}
}

func TestHub_ApplyOpportunity_PatchPathUsesCompositeKey(t *testing.T) {
	hub := NewHub(Config{SnapshotCacheMs: 500})
	opp := ArbOpportunity{EventID: "football:abc123", MarketType: "MATCH_WINNER", Line: 0.0}

	op := hub.ApplyOpportunity(opp)

	want := "/arb/football:abc123:MATCH_WINNER:0.00"
	if op.Path != want {
		t.Errorf("patch path = %q, want %q", op.Path, want)
	}
}

func TestHub_Snapshot_OnlySerializesArbOpportunity(t *testing.T) {
	hub := NewHub(Config{SnapshotCacheMs: 500})
	var opp ArbOpportunity
	json.Unmarshal([]byte(fullOppJSON), &opp)
	hub.ApplyOpportunity(opp)

	snap := hub.Snapshot()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(snap, &raw); err != nil {
		t.Fatalf("snapshot is not a valid JSON object: %v", err)
	}
	entry, ok := raw["football:a1b2c3d4e5f6:MATCH_WINNER:0.00"]
	if !ok {
		t.Fatal("snapshot missing composite key football:a1b2c3d4e5f6:MATCH_WINNER:0.00")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry, &fields); err != nil {
		t.Fatalf("snapshot entry is not a valid JSON object: %v", err)
	}
	if _, has := fields["lastSeen"]; has {
		t.Error("snapshot entry must not contain lastSeen field from arbEntry wrapper")
	}
	if _, has := fields["event_id"]; !has {
		t.Error("snapshot entry must contain event_id from ArbOpportunity")
	}
}

// ── Hub.evictStale ───────────────────────────────────────────────────────────

func TestHub_EvictStale_ExpiredEntryRemovedAndRemoveOpReturned(t *testing.T) {
	hub := NewHub(Config{SnapshotCacheMs: 500})
	var opp ArbOpportunity
	json.Unmarshal([]byte(fullOppJSON), &opp)
	hub.ApplyOpportunity(opp)

	// Backdate lastSeen to simulate an expired entry
	key := "football:a1b2c3d4e5f6:MATCH_WINNER:0.00"
	hub.stateMu.Lock()
	e := hub.state[key]
	e.lastSeen = time.Now().Add(-200 * time.Second)
	hub.state[key] = e
	hub.stateMu.Unlock()

	ops := hub.evictStale(120 * time.Second)

	if len(ops) != 1 {
		t.Fatalf("want 1 remove op, got %d", len(ops))
	}
	if ops[0].Op != "remove" {
		t.Errorf("op.Op = %q, want remove", ops[0].Op)
	}
	wantPath := "/arb/football:a1b2c3d4e5f6:MATCH_WINNER:0.00"
	if ops[0].Path != wantPath {
		t.Errorf("op.Path = %q, want %q", ops[0].Path, wantPath)
	}
	hub.stateMu.RLock()
	_, still := hub.state[key]
	hub.stateMu.RUnlock()
	if still {
		t.Error("evicted entry must be deleted from hub.state")
	}
}

func TestHub_EvictStale_FreshEntryNotEvicted(t *testing.T) {
	hub := NewHub(Config{SnapshotCacheMs: 500})
	var opp ArbOpportunity
	json.Unmarshal([]byte(fullOppJSON), &opp)
	hub.ApplyOpportunity(opp)

	ops := hub.evictStale(120 * time.Second)

	if len(ops) != 0 {
		t.Fatalf("fresh entry must not be evicted, got %d ops", len(ops))
	}
	hub.stateMu.RLock()
	_, ok := hub.state["football:a1b2c3d4e5f6:MATCH_WINNER:0.00"]
	hub.stateMu.RUnlock()
	if !ok {
		t.Error("fresh entry must remain in hub.state after eviction sweep")
	}
}

func TestHub_EvictStale_InvalidatesSnapshotCache(t *testing.T) {
	hub := NewHub(Config{SnapshotCacheMs: 500})
	var opp ArbOpportunity
	json.Unmarshal([]byte(fullOppJSON), &opp)
	hub.ApplyOpportunity(opp)
	// Warm the snapshot cache
	_ = hub.Snapshot()
	hub.stateMu.RLock()
	cachePopulated := hub.snapshotCache != nil
	hub.stateMu.RUnlock()
	if !cachePopulated {
		t.Fatal("precondition: snapshot cache should be populated after Snapshot()")
	}

	// Backdate and evict
	key := "football:a1b2c3d4e5f6:MATCH_WINNER:0.00"
	hub.stateMu.Lock()
	e := hub.state[key]
	e.lastSeen = time.Now().Add(-200 * time.Second)
	hub.state[key] = e
	hub.stateMu.Unlock()

	hub.evictStale(120 * time.Second)

	hub.stateMu.RLock()
	cacheNil := hub.snapshotCache == nil
	hub.stateMu.RUnlock()
	if !cacheNil {
		t.Error("evictStale must set snapshotCache to nil after removing entries")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func stateKeys(m map[string]arbEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
