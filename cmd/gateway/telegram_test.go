package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestHub_OnChange_ApplyRemoveEvict(t *testing.T) {
	hub := NewHub(Config{})
	var got []string
	hub.onChange = func(key string, _ ArbOpportunity, removed bool) {
		got = append(got, fmt.Sprintf("%s:%v", key, removed))
	}
	opp := ArbOpportunity{EventID: "e1", MarketType: "MATCH_WINNER", Sport: "tennis-live"}
	hub.ApplyOpportunity(opp)
	hub.RemoveOpportunity(opp)
	hub.RemoveOpportunity(opp) // already gone: no callback
	hub.ApplyOpportunity(opp)
	time.Sleep(5 * time.Millisecond)
	hub.evictStale(time.Millisecond, time.Millisecond)
	want := []string{"e1:MATCH_WINNER:0.00:false", "e1:MATCH_WINNER:0.00:true", "e1:MATCH_WINNER:0.00:false", "e1:MATCH_WINNER:0.00:true"}
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
