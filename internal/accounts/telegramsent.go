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
