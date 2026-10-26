package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// An id exercising every separator the engine may emit: ':' '|' '.'.
const valueID = "football:abc123:TOTAL_GOALS:2.50|OVER"

const fullValueJSON = `{"id":"football:abc123:TOTAL_GOALS:2.50|OVER","event_id":"football:abc123","sport":"football","name_home":"Arsenal","name_away":"Chelsea","league":"Premier League","start_time":1761500000000,"market_type":"TOTAL_GOALS","line":2.5,"player_name":null,"outcome_id":"OVER","outcome_label":"Over 2.5","reference_book":"pinnacle","reference_odds":1.95,"reference_margin_pct":2.6,"fair_prob":0.5013,"fair_odds":1.995,"best_book":"tipsport","best_odds":2.1,"best_market_name":"Počet gólů","best_match_url":null,"second_best_book":"fortuna","second_best_odds":2.02,"prices":{"tipsport":2.1,"fortuna":2.02},"ev_pct":5.27,"edge_vs_next_pct":3.96,"first_detected_at":1761400000000,"last_changed_at":1761400050000,"odds_updated_at":1761400049000,"reference_updated_at":1761400040000,"removed":false,"some_future_field":{"x":1}}`

func mustValueBet(t *testing.T, s string) ValueBet {
	t.Helper()
	var v ValueBet
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("unmarshal ValueBet: %v", err)
	}
	return v
}

func TestValueBet_DecodesRoutingFieldsAndRelaysVerbatim(t *testing.T) {
	v := mustValueBet(t, fullValueJSON)
	if v.ID != valueID || v.Sport != "football" || v.Removed {
		t.Fatalf("routing fields = %+v", v)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != fullValueJSON {
		t.Errorf("ValueBet must relay the engine payload verbatim\n got: %s\nwant: %s", out, fullValueJSON)
	}
}

func TestValueHub_ApplyStoresUnderIDAndPatchPathIsUnescaped(t *testing.T) {
	hub := NewValueHub(Config{SnapshotCacheMs: 500})
	op := hub.ApplyOpportunity(mustValueBet(t, fullValueJSON))

	if op.Op != "replace" {
		t.Errorf("op = %q, want replace", op.Op)
	}
	if want := "/value/" + valueID; op.Path != want {
		t.Errorf("path = %q, want %q", op.Path, want)
	}
	hub.stateMu.RLock()
	_, ok := hub.state[valueID]
	hub.stateMu.RUnlock()
	if !ok {
		t.Error("value bet must be stored under its id")
	}

	// The wire form of the op must carry the id unescaped and the full ValueBet.
	b, _ := json.Marshal(WSMessage{Type: "patch", Ops: []PatchOp{op}})
	var msg struct {
		Type string `json:"type"`
		Ops  []struct {
			Op    string          `json:"op"`
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		} `json:"ops"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "patch" || len(msg.Ops) != 1 || msg.Ops[0].Path != "/value/"+valueID {
		t.Fatalf("unexpected patch message: %s", b)
	}
	if string(msg.Ops[0].Value) != fullValueJSON {
		t.Errorf("patch value must be the verbatim ValueBet, got %s", msg.Ops[0].Value)
	}
}

func TestValueHub_ReplaceOverwritesSameID(t *testing.T) {
	hub := NewValueHub(Config{SnapshotCacheMs: 500})
	hub.ApplyOpportunity(mustValueBet(t, fullValueJSON))
	updated := strings.Replace(fullValueJSON, `"ev_pct":5.27`, `"ev_pct":6.5`, 1)
	hub.ApplyOpportunity(mustValueBet(t, updated))

	hub.stateMu.RLock()
	n := len(hub.state)
	got, _ := json.Marshal(hub.state[valueID].opp)
	hub.stateMu.RUnlock()
	if n != 1 {
		t.Fatalf("same id must replace, got %d entries", n)
	}
	if !strings.Contains(string(got), `"ev_pct":6.5`) {
		t.Errorf("stored value bet not replaced: %s", got)
	}
}

func TestValueHub_DistinctOutcomesDoNotCollide(t *testing.T) {
	hub := NewValueHub(Config{SnapshotCacheMs: 500})
	hub.ApplyOpportunity(mustValueBet(t, `{"id":"football:abc123:TOTAL_GOALS:2.50|OVER","sport":"football"}`))
	hub.ApplyOpportunity(mustValueBet(t, `{"id":"football:abc123:TOTAL_GOALS:2.50|UNDER","sport":"football"}`))
	hub.stateMu.RLock()
	defer hub.stateMu.RUnlock()
	if len(hub.state) != 2 {
		t.Fatalf("distinct ids must both be stored, got %d", len(hub.state))
	}
}

func TestValueHub_RemoveDropsPresentIDAndInvalidatesCache(t *testing.T) {
	hub := NewValueHub(Config{SnapshotCacheMs: 500})
	hub.ApplyOpportunity(mustValueBet(t, fullValueJSON))
	_ = hub.Snapshot() // warm cache

	removal := mustValueBet(t, `{"id":"`+valueID+`","removed":true}`)
	if !removal.Removed || !valueSpec.removed(removal) {
		t.Fatal("removed:true must decode as a removal")
	}
	op, ok := hub.RemoveOpportunity(removal)
	if !ok {
		t.Fatal("RemoveOpportunity must report ok=true for a present id")
	}
	if op.Op != "remove" || op.Path != "/value/"+valueID || op.Value != nil {
		t.Errorf("unexpected remove op: %+v", op)
	}
	hub.stateMu.RLock()
	_, still := hub.state[valueID]
	cacheNil := hub.snapshotCache == nil
	hub.stateMu.RUnlock()
	if still {
		t.Error("removed id must be deleted from state")
	}
	if !cacheNil {
		t.Error("RemoveOpportunity must invalidate the snapshot cache")
	}

	if _, ok := hub.RemoveOpportunity(removal); ok {
		t.Error("removing an absent id must be a no-op (ok=false)")
	}
}

func TestValueHub_EvictStale(t *testing.T) {
	hub := NewValueHub(Config{SnapshotCacheMs: 500})
	hub.ApplyOpportunity(mustValueBet(t, fullValueJSON))
	fresh := `{"id":"football:def456:MATCH_WINNER:0.00|DRAW","sport":"football"}`
	hub.ApplyOpportunity(mustValueBet(t, fresh))

	hub.stateMu.Lock()
	e := hub.state[valueID]
	e.lastSeen = time.Now().Add(-200 * time.Second)
	hub.state[valueID] = e
	hub.stateMu.Unlock()

	ops := hub.evictStale(120*time.Second, 120*time.Second)
	if len(ops) != 1 || ops[0].Op != "remove" || ops[0].Path != "/value/"+valueID {
		t.Fatalf("want exactly one remove op for the stale id, got %+v", ops)
	}
	hub.stateMu.RLock()
	_, staleStill := hub.state[valueID]
	_, freshStill := hub.state["football:def456:MATCH_WINNER:0.00|DRAW"]
	hub.stateMu.RUnlock()
	if staleStill {
		t.Error("stale value bet must be evicted")
	}
	if !freshStill {
		t.Error("fresh value bet must be retained")
	}
}

func TestValueBet_IsLive(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"is_live flag", `{"id":"a","event_id":"football:x","sport":"football","is_live":true}`, true},
		{"event_id fallback", `{"id":"a","event_id":"football-live:x","sport":"football"}`, true},
		{"prematch", `{"id":"a","event_id":"football:x","sport":"football","is_live":false}`, false},
		{"v1 payload", fullValueJSON, false},
		{"-live only in the event part", `{"id":"a","event_id":"football:team-live","sport":"football"}`, false},
	}
	for _, c := range cases {
		if got := mustValueBet(t, c.json).isLive(); got != c.want {
			t.Errorf("%s: isLive = %v, want %v", c.name, got, c.want)
		}
	}
}

// Live value bets use VALUE_LIVE_EVICTION_TTL_S, prematch VALUE_EVICTION_TTL_S.
func TestValueHub_EvictStale_LiveUsesShorterTTL(t *testing.T) {
	hub := NewValueHub(Config{SnapshotCacheMs: 500})
	const (
		liveFlag  = "football-live:l1:MATCH_WINNER:0.00|DRAW|tipsport"
		liveEvent = "football-live:l2:MATCH_WINNER:0.00|DRAW|tipsport"
		prematch  = "football:p1:MATCH_WINNER:0.00|DRAW|pinnacle"
	)
	hub.ApplyOpportunity(mustValueBet(t, `{"id":"`+liveFlag+`","event_id":"football-live:l1","sport":"football","is_live":true}`))
	hub.ApplyOpportunity(mustValueBet(t, `{"id":"`+liveEvent+`","event_id":"football-live:l2","sport":"football"}`))
	hub.ApplyOpportunity(mustValueBet(t, `{"id":"`+prematch+`","event_id":"football:p1","sport":"football","is_live":false}`))

	hub.stateMu.Lock()
	for k, e := range hub.state {
		e.lastSeen = time.Now().Add(-20 * time.Second) // > live 15s, < prematch 120s
		hub.state[k] = e
	}
	hub.stateMu.Unlock()

	ops := hub.evictStale(120*time.Second, 15*time.Second)
	got := map[string]bool{}
	for _, op := range ops {
		got[op.Path] = true
	}
	if len(ops) != 2 || !got["/value/"+liveFlag] || !got["/value/"+liveEvent] {
		t.Fatalf("want both live value bets evicted, got %+v", ops)
	}
	hub.stateMu.RLock()
	_, prematchStill := hub.state[prematch]
	hub.stateMu.RUnlock()
	if !prematchStill {
		t.Error("prematch value bet must survive the live TTL")
	}
}

func TestConfig_ValueLiveEvictionTTLDefault(t *testing.T) {
	t.Setenv("VALUE_LIVE_EVICTION_TTL_S", "")
	if got := configFromEnv().ValueLiveEvictionTTLS; got != 15 {
		t.Errorf("VALUE_LIVE_EVICTION_TTL_S default = %d, want 15", got)
	}
	t.Setenv("VALUE_LIVE_EVICTION_TTL_S", "7")
	if got := configFromEnv().ValueLiveEvictionTTLS; got != 7 {
		t.Errorf("VALUE_LIVE_EVICTION_TTL_S = %d, want 7", got)
	}
}

func TestValueHub_SnapshotIsFlatMapOfVerbatimValueBets(t *testing.T) {
	hub := NewValueHub(Config{SnapshotCacheMs: 500})
	hub.ApplyOpportunity(mustValueBet(t, fullValueJSON))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(hub.Snapshot(), &raw); err != nil {
		t.Fatalf("snapshot is not a JSON object: %v", err)
	}
	entry, ok := raw[valueID]
	if !ok {
		t.Fatalf("snapshot missing id %q: %v", valueID, raw)
	}
	if string(entry) != fullValueJSON {
		t.Errorf("snapshot entry must be the verbatim ValueBet, got %s", entry)
	}
}

func TestValueHub_IndependentOfArbHub(t *testing.T) {
	arb := NewHub(Config{SnapshotCacheMs: 500})
	val := NewValueHub(Config{SnapshotCacheMs: 500})
	val.ApplyOpportunity(mustValueBet(t, fullValueJSON))
	if string(arb.Snapshot()) != "{}" {
		t.Errorf("arb snapshot must not see value bets, got %s", arb.Snapshot())
	}
}

// ── routing ──────────────────────────────────────────────────────────────────

func TestMux_ValueSnapshotEndpoint(t *testing.T) {
	cfg := Config{SnapshotCacheMs: 500, MaxClients: 10}
	hub, val := NewHub(cfg), NewValueHub(cfg)
	val.ApplyOpportunity(mustValueBet(t, fullValueJSON))
	srv := httptest.NewServer(newMux(hub, val))
	defer srv.Close()

	for path, want := range map[string]string{
		"/api/value/snapshot": `{"` + valueID + `":` + fullValueJSON + `}`,
		"/api/arb/snapshot":   `{}`,
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s content-type = %q", path, resp.Header.Get("Content-Type"))
		}
		if string(body) != want {
			t.Errorf("%s body = %s, want %s", path, body, want)
		}
	}
}

// /ws/value must reach the value hub — not be swallowed by the /ws (arb) route —
// and speak the same snapshot → patch protocol.
func TestMux_WSValueServesValueHub(t *testing.T) {
	cfg := Config{SnapshotCacheMs: 500, MaxClients: 10}
	hub, val := NewHub(cfg), NewValueHub(cfg)
	hub.ApplyOpportunity(ArbOpportunity{EventID: "football:zzz", MarketType: "MATCH_WINNER"})
	val.ApplyOpportunity(mustValueBet(t, fullValueJSON))
	srv := httptest.NewServer(newMux(hub, val))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/value", nil)
	if err != nil {
		t.Fatalf("dial /ws/value: %v", err)
	}
	defer conn.CloseNow()

	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Type string                     `json:"type"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Type != "snapshot" || len(snap.Data) != 1 || snap.Data[valueID] == nil {
		t.Fatalf("unexpected /ws/value snapshot: %s", b)
	}

	// Register precedes the snapshot write, so the client is already subscribed.
	val.BroadcastPatch([]PatchOp{{Op: "remove", Path: "/value/" + valueID}})
	_, b, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"type":"patch","ops":[{"op":"remove","path":"/value/` + valueID + `"}]}`; string(b) != want {
		t.Errorf("patch = %s, want %s", b, want)
	}
}

// The existing arb route keeps serving the arb hub only.
func TestMux_WSStillServesArbHub(t *testing.T) {
	cfg := Config{SnapshotCacheMs: 500, MaxClients: 10}
	hub, val := NewHub(cfg), NewValueHub(cfg)
	val.ApplyOpportunity(mustValueBet(t, fullValueJSON))
	srv := httptest.NewServer(newMux(hub, val))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	defer conn.CloseNow()
	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"type":"snapshot","data":{}}`; strings.TrimSpace(string(b)) != want {
		t.Errorf("/ws snapshot = %s, want %s", b, want)
	}
}
