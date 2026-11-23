package main

import (
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func TestBotPrune_AlertsOlderThan7Days(t *testing.T) {
	_, svc, bots := newBotServer(t)
	uid := userIDOf(t, svc, newUser(t, svc, "u@example.com", accounts.RoleUser))
	clk := &botClock{t: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	insertAlert(t, svc, "old", uid, "window", "problem", clk.now().Add(-botRetention-time.Minute).UnixMilli())
	insertAlert(t, svc, "new", uid, "window", "problem", clk.now().Add(-time.Hour).UnixMilli())
	bots.prune()
	rows, err := svc.ListBotAlerts(uid, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != "new" {
		t.Fatalf("after prune: %+v %v", rows, err)
	}
}
