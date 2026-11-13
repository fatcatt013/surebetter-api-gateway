package accounts

import (
	"database/sql"
	"encoding/json"
)

// TelegramSentLeg is one leg of a posted arb: the book, the canonical outcome id, the odds and the
// stake the message showed (whole units of the row's currency).
type TelegramSentLeg struct {
	Bookmaker string
	OutcomeID string
	Odds      float64
	Stake     int64
}

// TelegramSentRow is one arb message the Telegram bot posted (telegram_sent_arbs + its legs).
// Times are Unix ms. Zero UserID, FirstDetectedAt and EndedAt mean unknown (stored as NULL);
// EndReason "" means the arb is still open (alive, as far as the bot knows).
type TelegramSentRow struct {
	ID              int64
	MessageID       int64
	SentAt          int64
	UserID          int64
	ArbKey          string // the gateway's oppStateKey (event_id:market_type:line), to tell re-detections apart
	FirstDetectedAt int64
	Sport           string // bare, without "-live"
	IsLive          bool
	MarketType      string
	Line            float64 // 0 = no line (or a 0 line), as in the arb key
	ProfitPct       float64 // the engine's profit_pct
	ShownProfitPct  float64 // the % the message showed (after stake rounding)
	Currency        string
	TotalStake      int64
	Payout          int64 // guaranteed return: TotalStake + Profit
	Profit          int64
	EndedAt         int64
	EndReason       string // removed, evicted, redetected, absent, unknown; "" = open
	Legs            []TelegramSentLeg
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// InsertTelegramSent stores a posted arb and its legs in one transaction and returns the row id.
func (s *Service) InsertTelegramSent(r TelegramSentRow) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO telegram_sent_arbs (sent_at, user_id, message_id, arb_key, first_detected_at, sport, is_live,
			market_type, line, profit_pct, shown_profit_pct, currency, total_stake, payout, profit)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.SentAt, nullIfZero(r.UserID), r.MessageID, r.ArbKey, nullIfZero(r.FirstDetectedAt), r.Sport, r.IsLive,
		r.MarketType, r.Line, r.ProfitPct, r.ShownProfitPct, r.Currency, r.TotalStake, r.Payout, r.Profit)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for i, l := range r.Legs {
		if _, err := tx.Exec(`INSERT INTO telegram_sent_legs (sent_id, idx, bookmaker, outcome_id, odds, stake) VALUES (?, ?, ?, ?, ?, ?)`,
			id, i, l.Bookmaker, l.OutcomeID, l.Odds, l.Stake); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// EndTelegramSent closes the open row of messageID: endedAt (0 = unknown, stored NULL) and reason.
// A row that already ended is left alone; no open row is not an error.
func (s *Service) EndTelegramSent(messageID, endedAt int64, reason string) error {
	_, err := s.db.Exec(`UPDATE telegram_sent_arbs SET ended_at = ?, end_reason = ?
		WHERE id = (SELECT MAX(id) FROM telegram_sent_arbs WHERE message_id = ? AND end_reason IS NULL)`,
		nullIfZero(endedAt), reason, messageID)
	return err
}

// SweepTelegramSent closes every open row whose message is not in keep (the messages the bot still
// tracks) as an unknown end, and returns how many it closed. The bot calls it once at start: what
// it no longer tracks will never get an end.
func (s *Service) SweepTelegramSent(keep []int64) (int64, error) {
	if keep == nil {
		keep = []int64{}
	}
	ids, err := json.Marshal(keep)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`UPDATE telegram_sent_arbs SET end_reason = 'unknown'
		WHERE end_reason IS NULL AND message_id NOT IN (SELECT value FROM json_each(?))`, string(ids))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TelegramSentBetween returns the rows sent in [from, to), oldest first, with their legs.
func (s *Service) TelegramSentBetween(from, to int64) ([]TelegramSentRow, error) {
	rows, err := s.db.Query(`SELECT id, message_id, sent_at, user_id, arb_key, first_detected_at, sport, is_live, market_type, line,
			profit_pct, shown_profit_pct, currency, total_stake, payout, profit, ended_at, end_reason
		FROM telegram_sent_arbs WHERE sent_at >= ? AND sent_at < ? ORDER BY sent_at, id`, from, to)
	if err != nil {
		return nil, err
	}
	out := []TelegramSentRow{}
	byID := map[int64]int{}
	for rows.Next() {
		var r TelegramSentRow
		var user, first, ended sql.NullInt64
		var reason sql.NullString
		if err := rows.Scan(&r.ID, &r.MessageID, &r.SentAt, &user, &r.ArbKey, &first, &r.Sport, &r.IsLive, &r.MarketType, &r.Line,
			&r.ProfitPct, &r.ShownProfitPct, &r.Currency, &r.TotalStake, &r.Payout, &r.Profit, &ended, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		r.UserID, r.FirstDetectedAt, r.EndedAt, r.EndReason = user.Int64, first.Int64, ended.Int64, reason.String
		byID[r.ID] = len(out)
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	legs, err := s.db.Query(`SELECT l.sent_id, l.bookmaker, l.outcome_id, l.odds, l.stake FROM telegram_sent_legs l
		JOIN telegram_sent_arbs a ON a.id = l.sent_id WHERE a.sent_at >= ? AND a.sent_at < ? ORDER BY l.sent_id, l.idx`, from, to)
	if err != nil {
		return nil, err
	}
	defer legs.Close()
	for legs.Next() {
		var id int64
		var l TelegramSentLeg
		if err := legs.Scan(&id, &l.Bookmaker, &l.OutcomeID, &l.Odds, &l.Stake); err != nil {
			return nil, err
		}
		if i, ok := byID[id]; ok {
			out[i].Legs = append(out[i].Legs, l)
		}
	}
	return out, legs.Err()
}

// TelegramStatRow is the part of a posted arb the statistics page needs: no ids of messages or
// users, no stakes, the legs reduced to their bookmakers (in the message's order). Zero
// FirstDetectedAt and EndedAt mean unknown; EndReason "" means still open.
type TelegramStatRow struct {
	ID              int64
	SentAt          int64
	FirstDetectedAt int64
	EndedAt         int64
	Sport           string
	IsLive          bool
	MarketType      string
	Line            float64
	ProfitPct       float64
	Profit          int64
	Currency        string
	EndReason       string
	Books           []string
}

// telegramStatsPage is how many arbs one statement of ScanTelegramStats reads.
var telegramStatsPage = 2000

// ScanTelegramStats calls fn for every arb sent in [from, to), oldest first (ties by id), and
// stops at fn's first error. The database has one connection, which the Telegram stats writer,
// logins and the bots share: so it reads short pages (keyset on sent_at, id) and calls fn only
// between statements, never holding the connection for the whole range. Repeated strings (sport,
// market, currency, reason, bookmaker) share one copy, which keeps a long range small in memory.
func (s *Service) ScanTelegramStats(from, to int64, fn func(TelegramStatRow) error) error {
	intern := map[string]string{}
	str := func(v string) string {
		if c, ok := intern[v]; ok {
			return c
		}
		intern[v] = v
		return v
	}
	afterAt, afterID := from, int64(0)
	for {
		page, err := s.telegramStatsPage(afterAt, afterID, to, str)
		if err != nil {
			return err
		}
		for _, r := range page {
			if err := fn(r); err != nil {
				return err
			}
		}
		if len(page) < telegramStatsPage {
			return nil
		}
		last := page[len(page)-1]
		afterAt, afterID = last.SentAt, last.ID
	}
}

// telegramStatsPage reads up to telegramStatsPage arbs after (afterAt, afterID) and before to,
// with their bookmakers.
func (s *Service) telegramStatsPage(afterAt, afterID, to int64, str func(string) string) ([]TelegramStatRow, error) {
	rows, err := s.db.Query(`SELECT id, sent_at, first_detected_at, ended_at, sport, is_live, market_type, line, profit_pct,
			profit, currency, end_reason
		FROM telegram_sent_arbs WHERE sent_at < ? AND (sent_at > ? OR (sent_at = ? AND id > ?))
		ORDER BY sent_at, id LIMIT ?`, to, afterAt, afterAt, afterID, telegramStatsPage)
	if err != nil {
		return nil, err
	}
	page := []TelegramStatRow{}
	ids := []int64{}
	for rows.Next() {
		var r TelegramStatRow
		var first, ended sql.NullInt64
		var reason sql.NullString
		if err := rows.Scan(&r.ID, &r.SentAt, &first, &ended, &r.Sport, &r.IsLive, &r.MarketType, &r.Line, &r.ProfitPct,
			&r.Profit, &r.Currency, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		r.FirstDetectedAt, r.EndedAt = first.Int64, ended.Int64
		r.Sport, r.MarketType, r.Currency, r.EndReason = str(r.Sport), str(r.MarketType), str(r.Currency), str(reason.String)
		page = append(page, r)
		ids = append(ids, r.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(page) == 0 {
		return page, err
	}
	idJSON, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	legs, err := s.db.Query(`SELECT sent_id, bookmaker FROM telegram_sent_legs
		WHERE sent_id IN (SELECT value FROM json_each(?)) ORDER BY sent_id, idx`, string(idJSON))
	if err != nil {
		return nil, err
	}
	defer legs.Close()
	byID := make(map[int64]int, len(page))
	for i, r := range page {
		byID[r.ID] = i
	}
	for legs.Next() {
		var id int64
		var book string
		if err := legs.Scan(&id, &book); err != nil {
			return nil, err
		}
		if i, ok := byID[id]; ok {
			page[i].Books = append(page[i].Books, str(book))
		}
	}
	return page, legs.Err()
}
