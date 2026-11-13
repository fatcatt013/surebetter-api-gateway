package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
	"github.com/surebetter/api-gateway/internal/telegram"
)

func TestHub_OnChange_ApplyRemoveEvict(t *testing.T) {
	hub := NewHub(Config{})
	var got []string
	hub.onChange = func(key string, _ ArbOpportunity, removed, evicted bool) {
		got = append(got, fmt.Sprintf("%s:%v:%v", key, removed, evicted))
	}
	opp := ArbOpportunity{EventID: "e1", MarketType: "MATCH_WINNER", Sport: "tennis-live"}
	hub.ApplyOpportunity(opp)
	hub.RemoveOpportunity(opp)
	hub.RemoveOpportunity(opp) // already gone: no callback
	hub.ApplyOpportunity(opp)
	time.Sleep(5 * time.Millisecond)
	hub.evictStale(time.Millisecond, time.Millisecond)
	want := []string{"e1:MATCH_WINNER:0.00:false:false", "e1:MATCH_WINNER:0.00:true:false", "e1:MATCH_WINNER:0.00:false:false", "e1:MATCH_WINNER:0.00:true:true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("callbacks = %v, want %v", got, want)
	}
}

func TestHub_Has(t *testing.T) {
	hub := NewHub(Config{})
	opp := ArbOpportunity{EventID: "e1", MarketType: "MATCH_WINNER"}
	if hub.Has(oppStateKey(opp)) {
		t.Fatal("empty hub")
	}
	hub.ApplyOpportunity(opp)
	if !hub.Has(oppStateKey(opp)) {
		t.Fatal("applied key must be present")
	}
}

func TestToTelegramArb_CopiesFields(t *testing.T) {
	o := ArbOpportunity{
		EventID: "e", Sport: "tennis-live", MarketType: "GAME_HANDICAP", Line: 3.5, ProfitPct: 2.96,
		DetectedAt: 3, FirstDetectedAt: 1, LastChangedAt: 2, NameHome: "A", NameAway: "B", League: "L", MarketName: "M", StartTime: 9,
		Stakes: []StakeAllocation{{Bookmaker: "betano", OutcomeID: "o", Label: "l", DecimalOdds: 2.1, StakePct: 49, MatchURL: "u"}},
	}
	a := toTelegramArb(o)
	if a.EventID != "e" || a.Line != 3.5 || a.FirstDetectedAt != 1 || a.LastChangedAt != 2 || a.StartTime != 9 ||
		len(a.Stakes) != 1 || a.Stakes[0].MatchURL != "u" || a.Stakes[0].StakePct != 49 || a.MarketName != "M" {
		t.Fatalf("toTelegramArb = %+v", a)
	}
}

func TestTelegramStatsSink_WritesTheAccountsDB(t *testing.T) {
	db, err := accounts.OpenDB(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := accounts.NewService(db, nil)
	sink := telegramStatsSink{svc}
	sent := telegram.SentArb{
		MessageID: 42, SentAt: 1000, UserID: 0, ArbKey: "k", FirstDetectedAt: 900, Sport: "tennis", IsLive: true,
		MarketType: "GAME_HANDICAP", Line: 3.5, ProfitPct: 2.96, ShownProfitPct: 2.96, Currency: "CZK",
		TotalStake: 10000, Payout: 10296, Profit: 296,
		Legs: []telegram.SentLeg{{Bookmaker: "betano", OutcomeID: "a:+3.5", Odds: 2.1, Stake: 4903}, {Bookmaker: "tipsport", OutcomeID: "a:-3.5", Odds: 2.02, Stake: 5097}},
	}
	if err := sink.InsertSent(sent); err != nil {
		t.Fatal(err)
	}
	other := sent
	other.MessageID, other.SentAt = 43, 2000
	if err := sink.InsertSent(other); err != nil {
		t.Fatal(err)
	}
	if err := sink.EndSent(telegram.SentEnd{MessageID: 42, EndedAt: 5000, Reason: telegram.EndRemoved}); err != nil {
		t.Fatal(err)
	}
	if err := sink.SweepSent(nil); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.TelegramSentBetween(0, 10_000)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %+v err %v", rows, err)
	}
	r := rows[0]
	if r.MessageID != 42 || r.Sport != "tennis" || !r.IsLive || r.Payout != 10296 || r.EndedAt != 5000 || r.EndReason != "removed" ||
		len(r.Legs) != 2 || r.Legs[1] != (accounts.TelegramSentLeg{Bookmaker: "tipsport", OutcomeID: "a:-3.5", Odds: 2.02, Stake: 5097}) {
		t.Fatalf("row %+v", r)
	}
	if rows[1].EndReason != "unknown" || rows[1].EndedAt != 0 {
		t.Fatalf("swept row %+v", rows[1])
	}
}

// memTelegramStore is an in-memory telegram.Store.
type memTelegramStore struct{ cfg telegram.BotConfig }

func (m *memTelegramStore) LoadConfig(context.Context) (telegram.BotConfig, error) { return m.cfg, nil }
func (m *memTelegramStore) SaveConfig(_ context.Context, c telegram.BotConfig) error {
	m.cfg = c
	return nil
}
func (m *memTelegramStore) LoadMessages(context.Context) (map[string]telegram.Tracked, error) {
	return map[string]telegram.Tracked{}, nil
}
func (m *memTelegramStore) SaveMessage(context.Context, string, telegram.Tracked) error { return nil }
func (m *memTelegramStore) DeleteMessage(context.Context, string) error                 { return nil }

type nopOutbox struct{}

func (nopOutbox) Enqueue(telegram.Job) {}

func TestAuth_TelegramConfigRecordsTheAdmin(t *testing.T) {
	db, err := accounts.OpenDB(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := accounts.NewService(db, nil)
	reg := newConnRegistry()
	svc.SetConnections(reg)
	render, err := telegram.NewRenderer("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	n := telegram.NewNotifier(&memTelegramStore{}, nopOutbox{}, render, telegram.Options{})
	cfg := Config{SnapshotCacheMs: 500, MaxClients: 10}
	srv := httptest.NewServer(newMux(NewHub(cfg), NewValueHub(cfg), n, &wsGate{acc: svc, reg: reg, required: true}))
	t.Cleanup(srv.Close)
	admin := newUser(t, svc, "a@example.com", accounts.RoleAdmin)
	p, ok, err := svc.AuthenticateSession(admin)
	if err != nil || !ok {
		t.Fatalf("session: %v %v", ok, err)
	}
	body, _ := json.Marshal(telegram.BotConfig{
		Enabled: true, Preset: &telegram.PresetRef{ID: "p1", Name: "P"},
		Filter:   telegram.Filter{LiveState: "all", ThreeLeg: "include"},
		Bankroll: telegram.DefaultBankroll,
	})
	if code, resp := request(t, srv, "PUT", "/api/telegram/config", admin, string(body), nil); code != 200 {
		t.Fatalf("PUT: %d %s", code, resp)
	}
	if n.Config().UpdatedBy != p.UserID || p.UserID == 0 {
		t.Fatalf("updated_by = %d, want %d", n.Config().UpdatedBy, p.UserID)
	}
}
