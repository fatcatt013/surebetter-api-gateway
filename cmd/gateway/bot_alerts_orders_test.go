package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// live switches the user's bots to Live.
func (r *alertRig) live() {
	setSettings(r.t, r.svc, r.b, r.uid, func(st *accounts.BotSettings) { st.DryRun = false })
}

// agentWithWindows attaches the agent with tipsport-1 and betano-1 idle.
func (r *alertRig) agentWithWindows() *Client {
	c := r.attach()
	r.status(alertWin("tipsport", 1, "idle", "bot", ""), alertWin("betano", 1, "idle", "bot", ""))
	return c
}

// order posts a tipsport/betano order that goes at once and returns its id.
func (r *alertRig) order(arbKey string) string {
	r.t.Helper()
	res, err := r.b.submit(r.uid, botOrderIn{ArbKey: arbKey, Match: "Slavia – Sparta", Legs: []botLeg{
		{Bookmaker: "tipsport", SourceID: "s1", Odds: 2.1, Stake: 100}, {Bookmaker: "betano", SourceID: "s2", Odds: 2.05, Stake: 102}}})
	if err != nil || res.State != "sent" {
		r.t.Fatalf("order: %+v %v", res, err)
	}
	return res.OrderID
}

func (r *alertRig) event(id, event, reason, flow string) {
	r.t.Helper()
	if err := r.b.event(r.uid, r.code.ID, botEventIn{OrderID: id, Event: event, Reason: reason}, flow); err != nil {
		r.t.Fatalf("event %s: %v", event, err)
	}
}

const where = "Tipsport 1, Betano 1 (Home PC)"

func TestBotAlerts_OutcomeUnknownOfARealOrder(t *testing.T) {
	r := newAlertRig(t)
	r.link()
	r.live()
	r.agentWithWindows()
	id := r.order("arb-1")
	r.alerts()
	r.event(id, "outcome_unknown", "no confirmation", "")
	got := r.alerts()
	want := "Order " + id[:8] + " · " + where + " · no confirmation · Bots paused"
	if len(got) != 1 || got[0].Kind != alertAttention || got[0].Severity != severityAttention || got[0].Title != titleOutcomeUnknown ||
		got[0].Detail != want || got[0].OrderID != id || got[0].AgentID != r.code.ID {
		t.Fatalf("alert: %+v, want detail %q", got, want)
	}
	jobs := r.out.take()
	if len(jobs) != 1 || !strings.Contains(jobs[0].Text, "— check the bookmaker before retrying") ||
		!strings.HasSuffix(jobs[0].Text, "\nhttps://sb.example/app/bots/orders?order="+id) {
		t.Fatalf("telegram: %+v", jobs)
	}
}

func TestBotAlerts_DryRunOrdersNeverAlert(t *testing.T) {
	r := newAlertRig(t)
	r.agentWithWindows() // the default mode is dry run
	id := r.order("arb-1")
	r.event(id, "accepted", "", "")
	r.event(id, "placed", "", "")
	id2 := r.order("arb-2") // last: an outcome unknown pauses the user, dry run or not
	r.event(id2, "accepted", "", "")
	r.event(id2, "outcome_unknown", "no confirmation", "")
	for _, a := range r.alerts() {
		if a.Kind == alertAttention || a.Kind == alertOrder {
			t.Fatalf("a dry run alerted: %+v", a)
		}
	}
}

func TestBotAlerts_OrderResults(t *testing.T) {
	r := newAlertRig(t)
	r.link()
	r.live()
	r.agentWithWindows()
	placed := r.order("arb-1")
	r.event(placed, "accepted", "", "")
	r.event(placed, "placed", "", "")
	refused := r.order("arb-2")
	r.event(refused, "stopped", "odds dropped before the start", "")
	stopped := r.order("arb-3")
	r.event(stopped, "accepted", "", "")
	r.event(stopped, "stopped", "odds dropped 2.10 → 2.05", "")
	cashed := r.order("arb-4")
	r.event(cashed, "accepted", "", "")
	r.event(cashed, "cashed_out", "rule 1", "")
	var got []botAlert
	for _, a := range r.alerts() {
		if a.Kind == alertOrder {
			got = append(got, a)
		}
	}
	if len(got) != 3 {
		t.Fatalf("order alerts: %+v (the stop before accepted is not one)", got)
	}
	if got[0].Title != "Placed" || got[0].Severity != severityInfo || got[0].OrderID != placed ||
		got[0].Detail != "Order "+placed[:8]+" · Slavia – Sparta · "+where {
		t.Fatalf("placed: %+v", got[0])
	}
	if got[1].Title != "Stopped" || got[1].Detail != "Order "+stopped[:8]+" · Slavia – Sparta · "+where+" · odds dropped 2.10 → 2.05" {
		t.Fatalf("stopped: %+v", got[1])
	}
	if got[2].Title != "Cashed out" || got[2].Detail != "Order "+cashed[:8]+" · Slavia – Sparta · "+where+" · rule 1" {
		t.Fatalf("cashed out: %+v", got[2])
	}
	if jobs := r.out.take(); len(jobs) != 0 {
		t.Fatalf("telegram (order default off): %+v", jobs)
	}
}

func TestBotAlerts_StuckOncePerOrder(t *testing.T) {
	r := newAlertRig(t)
	r.live()
	r.agentWithWindows()
	id := r.order("arb-1")
	r.event(id, "flow", "", compactFlow(json.RawMessage(`{"rule":"default","playbook":"overFirst","phase":2,"stuck":null}`)))
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("stuck null alerted: %+v", got)
	}
	stuck := compactFlow(json.RawMessage(`{"rule":"default","playbook":"overFirst","phase":2,"stuck":{"since":1,"choice":"wait"}}`))
	r.event(id, "flow", "", stuck)
	r.event(id, "flow", "", stuck)
	got := r.alerts()
	if len(got) != 1 || got[0].Title != titleStuck || got[0].Kind != alertAttention || got[0].Detail != "Order "+id[:8]+" · "+where {
		t.Fatalf("stuck: %+v", got)
	}
}

func TestBotAlerts_NoResultFromTheAgent(t *testing.T) {
	r := newAlertRig(t)
	r.live()
	r.agentWithWindows()
	id := r.order("arb-1")
	r.alerts()
	r.clk.advance(orderMemory)
	r.b.tick()
	got := r.alerts()
	if len(got) != 1 || got[0].Title != titleOutcomeUnknown || got[0].Detail != "Order "+id[:8]+" · "+where+" · No result from the agent · Bots paused" {
		t.Fatalf("no result: %+v", got)
	}
}

func TestBotAlerts_CheckedResolvesTheOrdersAttention(t *testing.T) {
	r := newAlertRig(t)
	r.live()
	r.agentWithWindows()
	id := r.order("arb-1")
	r.event(id, "outcome_unknown", "no confirmation", "")
	raised := r.alerts()[0]
	r.b.resolveOrderAlerts(r.uid, id)
	got := r.alerts()
	if len(got) != 1 || got[0].ID != raised.ID || got[0].ResolvedAt == 0 {
		t.Fatalf("resolved: %+v", got)
	}
}

func TestBotAlerts_GatewayRestartedRealOrders(t *testing.T) {
	r := newAlertRig(t)
	for _, o := range []accounts.BotOrderRow{
		{ID: "aaaaaaaa00000001", UserID: r.uid, ArbKey: "k1", CodeID: r.code.ID, State: "running", DryRun: false, Stake: 200, InfoJSON: "{}", LegsJSON: "[]", CreatedAt: 1},
		{ID: "bbbbbbbb00000002", UserID: r.uid, ArbKey: "k2", CodeID: r.code.ID, State: "running", DryRun: true, Stake: 200, InfoJSON: "{}", LegsJSON: "[]", CreatedAt: 1},
	} {
		if err := r.svc.InsertBotOrder(o); err != nil {
			t.Fatal(err)
		}
	}
	r.b.recoverOrders()
	got := r.alerts()
	if len(got) != 1 || got[0].OrderID != "aaaaaaaa00000001" || got[0].Title != titleOutcomeUnknown ||
		got[0].Detail != "Order aaaaaaaa · Home PC · Gateway restarted · Bots paused" {
		t.Fatalf("restart: %+v", got)
	}
}
