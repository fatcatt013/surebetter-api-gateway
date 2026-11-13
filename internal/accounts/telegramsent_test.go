package accounts

import (
	"errors"
	"reflect"
	"testing"
)

func sentRow(messageID, sentAt int64) TelegramSentRow {
	return TelegramSentRow{
		MessageID: messageID, SentAt: sentAt, UserID: 0, ArbKey: "tennis-live:e1:GAME_HANDICAP:3.50", FirstDetectedAt: sentAt - 38000,
		Sport: "tennis", IsLive: true, MarketType: "GAME_HANDICAP", Line: 3.5, ProfitPct: 2.96, ShownProfitPct: 2.96,
		Currency: "CZK", TotalStake: 10000, Payout: 10296, Profit: 296,
		Legs: []TelegramSentLeg{
			{Bookmaker: "betano", OutcomeID: "cody benton:+3.5", Odds: 2.10, Stake: 4903},
			{Bookmaker: "tipsport", OutcomeID: "cody benton:-3.5", Odds: 2.02, Stake: 5097},
		},
	}
}

func TestTelegramSent_InsertAndRead(t *testing.T) {
	s, _, _ := newTestService(t)
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	r := sentRow(42, 1_000_000)
	r.UserID = admin.UserID
	id, err := s.InsertTelegramSent(r)
	if err != nil || id == 0 {
		t.Fatalf("insert: id %d, err %v", id, err)
	}
	noUser := sentRow(43, 2_000_000)
	noUser.FirstDetectedAt = 0
	if _, err := s.InsertTelegramSent(noUser); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTelegramSent(sentRow(44, 3_000_000)); err != nil {
		t.Fatal(err)
	}

	rows, err := s.TelegramSentBetween(1_000_000, 3_000_000) // [from, to)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %+v, err %v", rows, err)
	}
	want := r
	want.ID = id
	if !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("row\n got %+v\nwant %+v", rows[0], want)
	}
	if rows[1].UserID != 0 || rows[1].FirstDetectedAt != 0 || rows[1].MessageID != 43 {
		t.Fatalf("unknown user / first detection should read back as 0: %+v", rows[1])
	}

	var nullUser, nullFirst int
	if err := s.db.QueryRow(`SELECT user_id IS NULL, first_detected_at IS NULL FROM telegram_sent_arbs WHERE message_id = 43`).Scan(&nullUser, &nullFirst); err != nil || nullUser != 1 || nullFirst != 1 {
		t.Fatalf("0 must be stored as NULL: user %d first %d err %v", nullUser, nullFirst, err)
	}
}

func TestTelegramSent_EndOnceAndUnknown(t *testing.T) {
	s, _, _ := newTestService(t)
	if _, err := s.InsertTelegramSent(sentRow(42, 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTelegramSent(sentRow(43, 2000)); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTelegramSent(42, 9000, "removed"); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTelegramSent(42, 9999, "absent"); err != nil { // already ended: unchanged
		t.Fatal(err)
	}
	if err := s.EndTelegramSent(43, 0, "unknown"); err != nil { // unknown end: no time
		t.Fatal(err)
	}
	rows, err := s.TelegramSentBetween(0, 10_000)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %+v err %v", rows, err)
	}
	if rows[0].EndedAt != 9000 || rows[0].EndReason != "removed" {
		t.Fatalf("ended row: %+v", rows[0])
	}
	if rows[1].EndedAt != 0 || rows[1].EndReason != "unknown" {
		t.Fatalf("unknown-end row: %+v", rows[1])
	}
	var null int
	if err := s.db.QueryRow(`SELECT ended_at IS NULL FROM telegram_sent_arbs WHERE message_id = 43`).Scan(&null); err != nil || null != 1 {
		t.Fatalf("unknown end must keep ended_at NULL: %d %v", null, err)
	}
	if err := s.EndTelegramSent(44, 1, "bogus"); err != nil {
		t.Fatalf("no row to end is not an error: %v", err)
	}
}

func TestTelegramSent_EndPicksTheOpenRowOfThatMessage(t *testing.T) {
	s, _, _ := newTestService(t)
	// An old row of message 7 (another chat, say) was already closed; the open one gets the end.
	if _, err := s.InsertTelegramSent(sentRow(7, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTelegramSent(7, 1500, "removed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTelegramSent(sentRow(7, 2000)); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTelegramSent(7, 2500, "evicted"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.TelegramSentBetween(0, 10_000)
	if rows[0].EndedAt != 1500 || rows[1].EndedAt != 2500 || rows[1].EndReason != "evicted" {
		t.Fatalf("rows %+v", rows)
	}
}

func TestTelegramSent_SweepClosesUntrackedOpenRows(t *testing.T) {
	s, _, _ := newTestService(t)
	for i, id := range []int64{1, 2, 3} {
		if _, err := s.InsertTelegramSent(sentRow(id, int64(1000*(i+1)))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EndTelegramSent(1, 1500, "removed"); err != nil {
		t.Fatal(err)
	}
	n, err := s.SweepTelegramSent([]int64{3}) // message 3 is still tracked
	if err != nil || n != 1 {
		t.Fatalf("swept %d, err %v", n, err)
	}
	rows, _ := s.TelegramSentBetween(0, 10_000)
	got := []string{rows[0].EndReason, rows[1].EndReason, rows[2].EndReason}
	if !reflect.DeepEqual(got, []string{"removed", "unknown", ""}) || rows[1].EndedAt != 0 || rows[0].EndedAt != 1500 {
		t.Fatalf("after sweep: %+v", rows)
	}
	if n, err := s.SweepTelegramSent(nil); err != nil || n != 1 { // nothing tracked: the last open row closes
		t.Fatalf("swept %d, err %v", n, err)
	}
}

func TestTelegramSent_LegsQueryableByBookmaker(t *testing.T) {
	s, _, _ := newTestService(t)
	if _, err := s.InsertTelegramSent(sentRow(42, 1000)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM telegram_sent_legs l JOIN telegram_sent_arbs a ON a.id = l.sent_id
		WHERE l.bookmaker = 'tipsport' AND a.sent_at >= 0 AND l.stake = 5097`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("legs by bookmaker: %d %v", n, err)
	}
}

func TestTelegramSent_ScanStatsPagesInSentOrderWithBooks(t *testing.T) {
	s, _, _ := newTestService(t)
	defer func(n int) { telegramStatsPage = n }(telegramStatsPage)
	telegramStatsPage = 2 // several pages
	for i, at := range []int64{5000, 1000, 3000, 3000, 9000, 7000} {
		r := sentRow(int64(i+1), at)
		if i == 2 {
			r.FirstDetectedAt = 0
			r.Legs = append(r.Legs, TelegramSentLeg{Bookmaker: "fortuna", OutcomeID: "x", Odds: 9, Stake: 1})
		}
		if i == 3 {
			r.Legs = nil
		}
		if _, err := s.InsertTelegramSent(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EndTelegramSent(2, 4000, "removed"); err != nil { // sent at 1000
		t.Fatal(err)
	}
	var got []TelegramStatRow
	err := s.ScanTelegramStats(1000, 9000, func(r TelegramStatRow) error { got = append(got, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var at []int64
	for _, r := range got {
		at = append(at, r.SentAt)
	}
	if !reflect.DeepEqual(at, []int64{1000, 3000, 3000, 5000, 7000}) { // [from, to), oldest first, ties by id
		t.Fatalf("sent_at %v", at)
	}
	want0 := TelegramStatRow{ID: got[0].ID, SentAt: 1000, FirstDetectedAt: 1000 - 38000, EndedAt: 4000, Sport: "tennis", IsLive: true,
		MarketType: "GAME_HANDICAP", Line: 3.5, ProfitPct: 2.96, Profit: 296, Currency: "CZK", EndReason: "removed",
		Books: []string{"betano", "tipsport"}}
	if !reflect.DeepEqual(got[0], want0) {
		t.Fatalf("row\n got %+v\nwant %+v", got[0], want0)
	}
	if got[1].FirstDetectedAt != 0 || !reflect.DeepEqual(got[1].Books, []string{"betano", "tipsport", "fortuna"}) || got[1].EndReason != "" {
		t.Fatalf("3-leg row with unknown detection: %+v", got[1])
	}
	if got[2].Books != nil {
		t.Fatalf("no legs: %+v", got[2])
	}
}

func TestTelegramSent_ScanStatsStopsOnCallbackError(t *testing.T) {
	s, _, _ := newTestService(t)
	for i := 0; i < 3; i++ {
		if _, err := s.InsertTelegramSent(sentRow(int64(i+1), int64(1000*(i+1)))); err != nil {
			t.Fatal(err)
		}
	}
	stop := errors.New("stop")
	n := 0
	err := s.ScanTelegramStats(0, 10_000, func(TelegramStatRow) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("n %d err %v", n, err)
	}
}
