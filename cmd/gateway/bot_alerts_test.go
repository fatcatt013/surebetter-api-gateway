package main

import (
	"encoding/json"
	"testing"
)

func TestAlertSettings_DefaultsAndStoredJSON(t *testing.T) {
	d := defaultAlertSettings()
	if d.Attention != (alertChannels{true, true}) || d.Window != (alertChannels{true, true}) || d.Agent != (alertChannels{true, false}) ||
		d.Order != (alertChannels{true, false}) {
		t.Fatalf("defaults: %+v", d)
	}
	if got := alertSettingsOf(""); got != d {
		t.Fatalf("empty: %+v", got)
	}
	if got := alertSettingsOf("not json"); got != d {
		t.Fatalf("unreadable: %+v", got)
	}
	got := alertSettingsOf(`{"window":{"app":false},"order":{"telegram":true}}`)
	want := d
	want.Window = alertChannels{App: false, Telegram: true}
	want.Order = alertChannels{App: true, Telegram: true}
	if got != want {
		t.Fatalf("partial row: %+v, want %+v (missing keys keep their default)", got, want)
	}
	if got.of(alertWindow) != want.Window || got.of(alertOrder) != want.Order || got.of(alertAgent) != want.Agent ||
		got.of(alertAttention) != want.Attention || got.of("weather") != (alertChannels{}) {
		t.Fatal("of() maps the kinds")
	}
	js, _ := json.Marshal(d)
	if string(js) != `{"attention":{"app":true,"telegram":true},"window":{"app":true,"telegram":true},"agent":{"app":true,"telegram":false},"order":{"app":true,"telegram":false}}` {
		t.Fatalf("JSON: %s", js)
	}
}

func TestJoinDetailAndAlertMessage(t *testing.T) {
	if got := joinDetail("Order 8d10c2aa", "", "Tipsport 1 (Home PC)", ""); got != "Order 8d10c2aa · Tipsport 1 (Home PC)" {
		t.Fatalf("joinDetail = %q", got)
	}
	var m struct {
		Type  string   `json:"type"`
		Alert botAlert `json:"alert"`
	}
	a := botAlert{ID: "9f2c4a1b0d3e5f67", Kind: alertWindow, Severity: severityProblem, AgentID: 12, WindowID: "tipsport-1", Title: "Logged out",
		Detail: "Tipsport 1 (Home PC)", CreatedAt: 5}
	if err := json.Unmarshal(alertMessage(a), &m); err != nil || m.Type != "alert" || m.Alert != a {
		t.Fatalf("message: %+v %v", m, err)
	}
	if row := a.row(3); alertOfRow(row) != a || row.UserID != 3 || row.AgentCodeID != 12 {
		t.Fatalf("row round trip: %+v", row)
	}
	js, _ := json.Marshal(a)
	if string(js) != `{"id":"9f2c4a1b0d3e5f67","kind":"window","severity":"problem","agentId":12,"windowId":"tipsport-1","orderId":"","title":"Logged out","detail":"Tipsport 1 (Home PC)","createdAt":5,"resolvedAt":0,"dismissedAt":0}` {
		t.Fatalf("alert JSON: %s", js)
	}
}
