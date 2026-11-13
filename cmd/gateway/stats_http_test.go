package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// seedSent stores a posted arb sent at `at`, detected 10 s before; reason "" leaves it open.
func seedSent(t *testing.T, svc *accounts.Service, msg int64, at time.Time, live bool, pct float64, reason string, books ...string) {
	t.Helper()
	r := accounts.TelegramSentRow{MessageID: msg, SentAt: ms(at), ArbKey: "k", FirstDetectedAt: ms(at) - 10_000, Sport: "tennis",
		IsLive: live, MarketType: "TOTAL_GAMES", Line: 21.5, ProfitPct: pct, ShownProfitPct: pct, Currency: "CZK",
		TotalStake: 10000, Payout: 10000 + int64(pct*100), Profit: int64(pct * 100)}
	for i, b := range books {
		r.Legs = append(r.Legs, accounts.TelegramSentLeg{Bookmaker: b, OutcomeID: "o" + itoa(int64(i)), Odds: 2, Stake: 5000})
	}
	if _, err := svc.InsertTelegramSent(r); err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		ended := ms(at) + 30_000
		if reason == "unknown" {
			ended = 0
		}
		if err := svc.EndTelegramSent(msg, ended, reason); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatsHTTP_AdminOnly(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	admin := newUser(t, svc, "admin@example.com", accounts.RoleAdmin)
	user := newUser(t, svc, "user@example.com", accounts.RoleUser)
	for _, path := range []string{"/api/admin/stats/arbs", "/api/admin/stats/arbs.csv"} {
		if s, b := request(t, srv, "GET", path, "", "", nil); s != 401 || !strings.Contains(b, `"unauthorized"`) {
			t.Errorf("%s without a session: %d %s", path, s, b)
		}
		if s, b := request(t, srv, "GET", path, user, "", nil); s != 403 || !strings.Contains(b, `"forbidden"`) {
			t.Errorf("%s as a user: %d %s", path, s, b)
		}
		if s, b := request(t, srv, "GET", path, admin, "", nil); s != 200 {
			t.Errorf("%s as the admin: %d %s", path, s, b)
		}
		if s, _ := request(t, srv, "POST", path, admin, "{}", nil); s != 405 {
			t.Errorf("POST %s: %d", path, s)
		}
	}
	if s, _ := request(t, srv, "GET", "/api/admin/users", admin, "", nil); s != 200 {
		t.Fatalf("the accounts' admin API still answers: %d", s)
	}
}

func TestStatsHTTP_BadQuery(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	admin := newUser(t, svc, "admin@example.com", accounts.RoleAdmin)
	for _, q := range []string{"?from=x", "?include=A", "?live=1", "?bucket=hour&from=0&to=" + itoa(2001*3600_000)} {
		for _, path := range []string{"/api/admin/stats/arbs", "/api/admin/stats/arbs.csv"} {
			if s, b := request(t, srv, "GET", path+q, admin, "", nil); s != 400 || !strings.Contains(b, `"bad_query"`) {
				t.Errorf("%s%s: %d %s", path, q, s, b)
			}
		}
	}
}

func TestStatsHTTP_ArbsFromTheDatabase(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	admin := newUser(t, svc, "admin@example.com", accounts.RoleAdmin)
	day := pragueAt(2026, 11, 12, 0, 0)
	seedSent(t, svc, 1, day.Add(-time.Minute), true, 9, "removed", "betano", "tipsport") // before the range: out, not even "seen"
	seedSent(t, svc, 2, day.Add(10*time.Hour), true, 2, "removed", "fortuna", "tipsport")
	seedSent(t, svc, 3, day.Add(11*time.Hour), false, 4, "evicted", "fortuna", "tipsport", "pinnacle")
	seedSent(t, svc, 4, day.Add(12*time.Hour), true, 6, "", "chance", "tipsport")
	seedSent(t, svc, 5, day.Add(24*time.Hour), true, 1, "removed", "fortuna", "tipsport") // at to: out
	q := "?from=" + itoa(ms(day)) + "&to=" + itoa(ms(day.Add(24*time.Hour))) + "&exclude=chance"
	s, b := request(t, srv, "GET", "/api/admin/stats/arbs"+q, admin, "", nil)
	if s != 200 {
		t.Fatalf("%d %s", s, b)
	}
	var a statsAnswer
	if err := json.Unmarshal([]byte(b), &a); err != nil {
		t.Fatal(err)
	}
	if a.KPIs.Arbs != 2 || *a.KPIs.AvgProfitPct != 3 || a.KPIs.Profit != 600 || *a.KPIs.MedianLifeS != 40 || *a.KPIs.MedianAfterSendS != 30 {
		t.Fatalf("kpis %+v", a.KPIs)
	}
	if got := jsonOf(t, a.Bookmakers); got != `[{"id":"tipsport","legs":2},{"id":"fortuna","legs":2},{"id":"pinnacle","legs":1},{"id":"chance","legs":0}]` &&
		got != `[{"id":"fortuna","legs":2},{"id":"tipsport","legs":2},{"id":"pinnacle","legs":1},{"id":"chance","legs":0}]` {
		t.Fatalf("books %s", got)
	}
	if len(a.Rows) != 2 || a.Rows[0].SentAt != ms(day.Add(11*time.Hour)) || a.Rows[0].IsLive || a.NextCursor != nil ||
		strings.Join(a.Rows[0].Bookmakers, ",") != "fortuna,tipsport,pinnacle" {
		t.Fatalf("rows %s", jsonOf(t, a.Rows))
	}
	if len(a.Series.Points) != 1 || a.Series.Points[0].Live != 1 || a.Series.Points[0].Prematch != 1 || a.Heatmap[3][11] != 1 {
		t.Fatalf("series %s heatmap %v", jsonOf(t, a.Series), a.Heatmap)
	}
	if !strings.Contains(b, `"nextCursor":null`) || !strings.Contains(b, `"endReasons":{"removed":1,"evicted":1,`) {
		t.Fatalf("body %s", b)
	}
}

func TestStatsHTTP_CursorPages(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	admin := newUser(t, svc, "admin@example.com", accounts.RoleAdmin)
	now := time.Now()
	for i := 0; i < 55; i++ {
		seedSent(t, svc, int64(i+1), now.Add(-time.Hour).Add(time.Duration(i)*time.Second), true, 2, "removed", "fortuna", "tipsport")
	}
	var a statsAnswer
	_, b := request(t, srv, "GET", "/api/admin/stats/arbs", admin, "", nil) // default range: the last 14 days
	if err := json.Unmarshal([]byte(b), &a); err != nil || len(a.Rows) != 50 || a.NextCursor == nil || a.KPIs.Arbs != 55 {
		t.Fatalf("page 1: %v %s", err, b)
	}
	newest := a.Rows[0].ID
	_, b = request(t, srv, "GET", "/api/admin/stats/arbs?cursor="+*a.NextCursor, admin, "", nil)
	a = statsAnswer{}
	if err := json.Unmarshal([]byte(b), &a); err != nil || len(a.Rows) != 5 || a.NextCursor != nil || a.Rows[0].ID != newest-50 {
		t.Fatalf("page 2: %v %s", err, b)
	}
}

func TestStatsHTTP_CSV(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	admin := newUser(t, svc, "admin@example.com", accounts.RoleAdmin)
	day := pragueAt(2026, 7, 1, 0, 0) // summer: +02:00
	seedSent(t, svc, 1, day.Add(10*time.Hour+30*time.Second), true, 2.5, "removed", "fortuna", "tipsport")
	seedSent(t, svc, 2, day.Add(11*time.Hour), false, 4, "", "betano", "tipsport")
	seedSent(t, svc, 3, day.Add(12*time.Hour), true, 7, "unknown", "betano", "tipsport")
	q := "?from=" + itoa(ms(day)) + "&to=" + itoa(ms(day.Add(24*time.Hour))) + "&maxProfit=5"
	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/stats/arbs.csv"+q, nil)
	req.Header.Set("Cookie", accounts.CookieName+"="+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="surebetter-arbs.csv"` {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	want := "sent_at,sport,live,market,line,bookmakers,profit_pct,profit,currency,lifetime_s,after_send_s,end_reason\r\n" +
		"2026-07-01T11:00:00+02:00,tennis,prematch,TOTAL_GAMES,21.5,betano|tipsport,4,400,CZK,,,open\r\n" +
		"2026-07-01T10:00:30+02:00,tennis,live,TOTAL_GAMES,21.5,fortuna|tipsport,2.5,250,CZK,40,30,removed\r\n"
	if string(body) != want {
		t.Fatalf("csv\n got %q\nwant %q", body, want)
	}
}

func TestStatsHTTP_TooManyRows(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	admin := newUser(t, svc, "admin@example.com", accounts.RoleAdmin)
	defer func(n int) { statsMaxRows = n }(statsMaxRows)
	statsMaxRows = 2
	now := time.Now()
	for i := 0; i < 3; i++ {
		seedSent(t, svc, int64(i+1), now.Add(-time.Duration(i+1)*time.Minute), true, 2, "removed", "fortuna", "tipsport")
	}
	if s, b := request(t, srv, "GET", "/api/admin/stats/arbs", admin, "", nil); s != 413 || !strings.Contains(b, `"too_many_rows"`) {
		t.Fatalf("%d %s", s, b)
	}
	if s, _ := request(t, srv, "GET", "/api/admin/stats/arbs?live=prematch", admin, "", nil); s != 200 { // the cap counts kept rows
		t.Fatalf("filtered: %d", s)
	}
}
