package main

import (
	"strings"
	"testing"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// fleetWindow is window id of the user's only agent in the Fleet.
func fleetWindow(t *testing.T, fleet []agentView, id string) botWindow {
	t.Helper()
	if len(fleet) != 1 {
		t.Fatalf("fleet: %+v", fleet)
	}
	for _, w := range fleet[0].Windows {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("no window %s in %+v", id, fleet[0].Windows)
	return botWindow{}
}

// A leg_step "released" (rule 5: the leg a single-leg pair never plays) frees that leg's window at
// once: the Fleet no longer shows it busy, another order may take it, and the leg is placed:false.
// The order's end later leaves the window's new order alone.
func TestBotSched_ReleasedLegFreesItsWindow(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	setSettings(t, svc, bots, userIDOf(t, svc, token), func(st *accounts.BotSettings) { st.OrdersAtOnce = 2 })
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("tipsport", 2, "idle"), win("betano", 1, "idle")), app)
	a := placeOrder(t, srv, token, "arb-1", bot, app)
	if a.Legs[1].WindowID != "betano-1" {
		t.Fatalf("order A: %+v", a.Legs)
	}
	if w := fleetWindow(t, getFleet(t, srv, token), "betano-1"); w.State != "busy" || w.OrderID != a.OrderID {
		t.Fatalf("before release: %+v", w)
	}
	b := postOrderOK(t, srv, token, tipBetOrder("arb-2"))
	if b.State != "waiting" {
		t.Fatalf("order B: %+v", b)
	}
	expectEvent(t, app, b.OrderID, "waiting", "Needs a free Betano window")
	expectBot(t, app, "on the waiting order")

	if s, body := postEvent(t, srv, code, `{"order_id":"`+a.OrderID+`","event":"leg_step","bookmaker":"betano","step":"released"}`); s != 204 {
		t.Fatalf("released: %d %s", s, body)
	}
	if m := expectEvent(t, app, a.OrderID, "leg_step", ""); m["bookmaker"] != "betano" || m["step"] != "released" {
		t.Fatalf("relay: %v", m)
	}
	// The waiting order takes the released window at once.
	o := readOrder(t, bot)
	if o.OrderID != b.OrderID || o.Legs[0].WindowID != "tipsport-2" || o.Legs[1].WindowID != "betano-1" {
		t.Fatalf("order B sent: %+v", o)
	}
	expectEvent(t, app, b.OrderID, "sent", "")
	expectBot(t, app, "on the release")
	if w := fleetWindow(t, getFleet(t, srv, token), "betano-1"); w.State != "busy" || w.OrderID != b.OrderID {
		t.Fatalf("after release: %+v", w)
	}
	bots.mu.Lock()
	leg := bots.orders[a.OrderID].legs[1]
	bots.mu.Unlock()
	if !leg.Released || leg.Placed == nil || *leg.Placed || leg.Step != "released" {
		t.Fatalf("released leg in memory: %+v", leg)
	}
	var js string
	db.QueryRow(`SELECT legs_json FROM bot_orders WHERE id = ?`, a.OrderID).Scan(&js)
	if _, _, _, _, legs := orderRow(t, db, a.OrderID); !legs[1].Released || legs[1].Placed == nil || *legs[1].Placed || legs[0].Released ||
		legs[0].Placed != nil || !strings.Contains(js, `"placed":false`) {
		t.Fatalf("legs_json of a running order: %s", js)
	}
	// Released again (or a later step of that leg): relayed, the window stays B's.
	if s, body := postEvent(t, srv, code, `{"order_id":"`+a.OrderID+`","event":"leg_step","bookmaker":"betano","step":"released"}`); s != 204 {
		t.Fatalf("released again: %d %s", s, body)
	}
	expectEvent(t, app, a.OrderID, "leg_step", "")

	// A ends: its tipsport window is free, betano-1 stays B's.
	if s, body := postEvent(t, srv, code, `{"order_id":"`+a.OrderID+`","event":"placed","legs":[{"bookmaker":"tipsport","stake":100,"odds":0,"placed":true}]}`); s != 204 {
		t.Fatalf("final: %d %s", s, body)
	}
	expectEvent(t, app, a.OrderID, "placed", "")
	expectBot(t, app, "after the final event")
	fleet := getFleet(t, srv, token)
	if w := fleetWindow(t, fleet, "betano-1"); w.State != "busy" || w.OrderID != b.OrderID {
		t.Fatalf("betano-1 after A ended: %+v", w)
	}
	if w := fleetWindow(t, fleet, "tipsport-1"); w.State != "idle" || w.OrderID != "" {
		t.Fatalf("tipsport-1 after A ended: %+v", w)
	}
	uid, codeID := userIDOf(t, svc, token), codeIDOf(t, bots)
	bots.mu.Lock()
	_, betanoIdle := bots.agents[uid][codeID].idleSince["betano-1"]
	bots.mu.Unlock()
	if betanoIdle {
		t.Fatal("the end of A marked B's window idle")
	}
	if state, _, _, _, legs := orderRow(t, db, a.OrderID); state != "placed" || legs[1].Placed == nil || *legs[1].Placed {
		t.Fatalf("A's row: %s %+v", state, legs)
	}
}

// codeIDOf is the code id of the hub's only online agent.
func codeIDOf(t *testing.T, bots *botHub) int64 {
	t.Helper()
	bots.mu.Lock()
	defer bots.mu.Unlock()
	for _, agents := range bots.agents {
		for id := range agents {
			return id
		}
	}
	t.Fatal("no agent online")
	return 0
}

// A "released" step for an unknown, ended or another agent's order, or for a bookmaker that is not
// one of the order's legs, is ignored (204, nothing relayed). Other steps keep their 404.
func TestBotSched_ReleasedForUnknownOrderOrLegIsIgnored(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startBot(t, srv, code, app)
	o := placeOrder(t, srv, token, "arb-1", bot, app)
	for name, body := range map[string]string{
		"unknown order": `{"order_id":"0123456789abcdef","event":"leg_step","bookmaker":"betano","step":"released"}`,
		"unknown leg":   `{"order_id":"` + o.OrderID + `","event":"leg_step","bookmaker":"fortuna","step":"released"}`,
	} {
		if s, b := postEvent(t, srv, code, body); s != 204 {
			t.Errorf("%s: %d %s, want 204", name, s, b)
		}
	}
	expectNoMessage(t, app, "app (an ignored release relays nothing)")
	if s, b := postEvent(t, srv, code, `{"order_id":"0123456789abcdef","event":"leg_step","bookmaker":"betano","step":"navigate"}`); s != 404 ||
		!strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("another step of an unknown order: %d %s", s, b)
	}
	if w := fleetWindow(t, getFleet(t, srv, token), "betano-1"); w.State != "busy" || w.OrderID != o.OrderID {
		t.Fatalf("an ignored release keeps the reservation: %+v", w)
	}
}
