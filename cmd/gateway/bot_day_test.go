package main

import (
	"testing"
	"time"
)

func TestBotDay_PragueDays(t *testing.T) {
	utc := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name, at, from, to string
	}{
		{"winter (UTC+1)", "2026-11-13T22:59:59Z", "2026-11-12T23:00:00Z", "2026-11-13T23:00:00Z"},
		{"winter, Prague midnight", "2026-11-13T23:00:00Z", "2026-11-13T23:00:00Z", "2026-11-14T23:00:00Z"},
		{"summer (UTC+2)", "2026-07-01T21:59:59Z", "2026-06-30T22:00:00Z", "2026-07-01T22:00:00Z"},
		{"spring forward: a 23 h day", "2026-03-29T12:00:00Z", "2026-03-28T23:00:00Z", "2026-03-29T22:00:00Z"},
		{"fall back: a 25 h day", "2026-10-25T12:00:00Z", "2026-10-24T22:00:00Z", "2026-10-25T23:00:00Z"},
	}
	for _, c := range cases {
		from, to := pragueDayOf(utc(c.at))
		if !from.Equal(utc(c.from)) || !to.Equal(utc(c.to)) {
			t.Errorf("%s: %s – %s, want %s – %s", c.name, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), c.from, c.to)
		}
	}
}

func TestBotDay_ParseDay(t *testing.T) {
	from, to, ok := parsePragueDay("2026-10-25")
	if !ok || from.UTC().Format(time.RFC3339) != "2026-10-24T22:00:00Z" || to.UTC().Format(time.RFC3339) != "2026-10-25T23:00:00Z" {
		t.Fatalf("2026-10-25: %v %v %v", from, to, ok)
	}
	for _, bad := range []string{"", "2026-13-01", "2026-02-30", "26-11-13", "2026-11-13T00:00", "2026-1-5", " 2026-11-13"} {
		if _, _, ok := parsePragueDay(bad); ok {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestBotDay_BookLabel(t *testing.T) {
	for in, want := range map[string]string{"tipsport": "Tipsport", "sazkabet": "SazkaBet", "merkurxtip": "MerkurXtip",
		"synottip": "SynotTip", "kingsbet": "Kingsbet", "testsite": "Testsite", "": ""} {
		if got := bookLabel(in); got != want {
			t.Errorf("bookLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
