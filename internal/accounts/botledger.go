package accounts

import "database/sql"

// The bot bankroll's storage (migration 15): balance reads, each account's point 0 and the ledger of really placed
// legs. The money logic lives in the gateway (cmd/gateway/bot_bankroll.go); these are plain reads and writes.

// BotBalanceRow is one stored balance read of a window (an account = code id + window id). At is the read's time.
type BotBalanceRow struct {
	UserID    int64
	CodeID    int64
	WindowID  string
	Bookmaker string
	Amount    float64
	At        int64
}

// BotAccountRow is an account's point 0: its first stored balance read.
type BotAccountRow struct {
	UserID      int64
	CodeID      int64
	WindowID    string
	Bookmaker   string
	StartAmount float64
	StartAt     int64
}

// BotLedgerLeg is one really placed leg of a live order (bot_ledger_legs). Leg is its index in the order's legs_json.
// TicketID "" = none (NULL); Payout and SettledAt nil while open.
type BotLedgerLeg struct {
	OrderID      string
	Leg          int
	UserID       int64
	CodeID       int64
	WindowID     string
	Bookmaker    string
	Source       string // the order's: app or auto
	Match        string
	Market       string
	MarketType   string
	Line         *float64
	Label        string
	Odds         float64
	Stake        float64
	PlacedAt     int64
	Voidable     bool
	TicketID     string
	Result       string // open, won, lost, void, half_won, half_lost, cashed_out or not_placed (migration 16)
	Payout       *float64
	SettledAt    *int64
	ResultSource string // "", manual or bot
	UpdatedAt    int64
}

// InsertBotBalance stores one balance read and, in the same transaction, the account's point 0 when it has none yet.
func (s *Service) InsertBotBalance(r BotBalanceRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO bot_balances (user_id, code_id, window_id, bookmaker, amount, at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.UserID, r.CodeID, r.WindowID, r.Bookmaker, r.Amount, r.At); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO bot_accounts (user_id, code_id, window_id, bookmaker, start_amount, start_at)
		VALUES (?, ?, ?, ?, ?, ?)`, r.UserID, r.CodeID, r.WindowID, r.Bookmaker, r.Amount, r.At); err != nil {
		return err
	}
	return tx.Commit()
}

func scanBalances(rows *sql.Rows, err error) ([]BotBalanceRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BotBalanceRow{}
	for rows.Next() {
		var r BotBalanceRow
		if err := rows.Scan(&r.UserID, &r.CodeID, &r.WindowID, &r.Bookmaker, &r.Amount, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestBotBalances returns the newest stored read of each of userID's accounts (of code codeID; 0 = every code), by
// code id and window id.
func (s *Service) LatestBotBalances(userID, codeID int64) ([]BotBalanceRow, error) {
	return scanBalances(s.db.Query(`SELECT b.user_id, b.code_id, b.window_id, b.bookmaker, b.amount, b.at FROM bot_balances b
		WHERE b.user_id = ? AND (? = 0 OR b.code_id = ?) AND b.rowid = (SELECT b2.rowid FROM bot_balances b2
			WHERE b2.code_id = b.code_id AND b2.window_id = b.window_id ORDER BY b2.at DESC, b2.rowid DESC LIMIT 1)
		ORDER BY b.code_id, b.window_id`, userID, codeID, codeID))
}

// BotBalances returns every stored read of userID, oldest first.
func (s *Service) BotBalances(userID int64) ([]BotBalanceRow, error) {
	return scanBalances(s.db.Query(`SELECT user_id, code_id, window_id, bookmaker, amount, at FROM bot_balances
		WHERE user_id = ? ORDER BY at, rowid`, userID))
}

// BotAccounts returns userID's accounts with their point 0, by code id and window id.
func (s *Service) BotAccounts(userID int64) ([]BotAccountRow, error) {
	rows, err := s.db.Query(`SELECT user_id, code_id, window_id, bookmaker, start_amount, start_at FROM bot_accounts
		WHERE user_id = ? ORDER BY code_id, window_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BotAccountRow{}
	for rows.Next() {
		var r BotAccountRow
		if err := rows.Scan(&r.UserID, &r.CodeID, &r.WindowID, &r.Bookmaker, &r.StartAmount, &r.StartAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertBotLedgerLegs stores legs in one transaction; a leg already stored (order id + leg) is left as it is.
func (s *Service) InsertBotLedgerLegs(legs []BotLedgerLeg) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, l := range legs {
		var ticket any
		if l.TicketID != "" {
			ticket = l.TicketID
		}
		result := l.Result
		if result == "" {
			result = "open"
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO bot_ledger_legs (order_id, leg, user_id, code_id, window_id, bookmaker, source, match,
				market, market_type, line, label, odds, stake, placed_at, voidable, ticket_id, result, payout, settled_at, result_source, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.OrderID, l.Leg, l.UserID, l.CodeID, l.WindowID, l.Bookmaker, l.Source, l.Match, l.Market, l.MarketType, l.Line, l.Label,
			l.Odds, l.Stake, l.PlacedAt, l.Voidable, ticket, result, l.Payout, l.SettledAt, l.ResultSource, l.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const ledgerColumns = `order_id, leg, user_id, code_id, window_id, bookmaker, source, match, market, market_type, line, label,
	odds, stake, placed_at, voidable, COALESCE(ticket_id, ''), result, payout, settled_at, result_source, updated_at`

// BotLedgerLegs returns every ledger leg of userID, by order id and leg.
func (s *Service) BotLedgerLegs(userID int64) ([]BotLedgerLeg, error) {
	return scanLedger(s.db.Query(`SELECT `+ledgerColumns+` FROM bot_ledger_legs WHERE user_id = ? ORDER BY order_id, leg`, userID))
}

// BotLedgerLegOf returns userID's ledger leg (orderID, leg); found false when there is none.
func (s *Service) BotLedgerLegOf(userID int64, orderID string, leg int) (BotLedgerLeg, bool, error) {
	legs, err := scanLedger(s.db.Query(`SELECT `+ledgerColumns+` FROM bot_ledger_legs WHERE user_id = ? AND order_id = ? AND leg = ?`,
		userID, orderID, leg))
	if err != nil || len(legs) == 0 {
		return BotLedgerLeg{}, false, err
	}
	return legs[0], true, nil
}

func scanLedger(rows *sql.Rows, err error) ([]BotLedgerLeg, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BotLedgerLeg{}
	for rows.Next() {
		var l BotLedgerLeg
		var line, payout sql.NullFloat64
		var settled sql.NullInt64
		if err := rows.Scan(&l.OrderID, &l.Leg, &l.UserID, &l.CodeID, &l.WindowID, &l.Bookmaker, &l.Source, &l.Match, &l.Market,
			&l.MarketType, &line, &l.Label, &l.Odds, &l.Stake, &l.PlacedAt, &l.Voidable, &l.TicketID, &l.Result, &payout, &settled,
			&l.ResultSource, &l.UpdatedAt); err != nil {
			return nil, err
		}
		if line.Valid {
			l.Line = &line.Float64
		}
		if payout.Valid {
			l.Payout = &payout.Float64
		}
		if settled.Valid {
			l.SettledAt = &settled.Int64
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetBotLedgerResult sets userID's ledger leg (orderID, leg) to result with payout and settledAt (nil = NULL, for
// open), its result source and updated_at. false: no such leg of userID.
func (s *Service) SetBotLedgerResult(userID int64, orderID string, leg int, result string, payout *float64, settledAt *int64,
	source string, updatedAt int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE bot_ledger_legs SET result = ?, payout = ?, settled_at = ?, result_source = ?, updated_at = ?
		WHERE order_id = ? AND leg = ? AND user_id = ?`, result, payout, settledAt, source, updatedAt, orderID, leg, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// stuckFlow matches a stored (compact) flow whose stuck is an object.
const stuckFlow = `%"stuck":{%`

// BotLedgerOrders returns userID's non-dry orders the ledger reads: those with a ledger leg, and the ended ones that
// need a check (outcome_unknown, or a last flow with stuck set), by id.
func (s *Service) BotLedgerOrders(userID int64) ([]BotOrderRow, error) {
	rows, err := s.db.Query(`SELECT `+botOrderColumns+` FROM bot_orders o LEFT JOIN extension_codes c ON c.id = o.code_id
		WHERE o.user_id = ? AND o.dry_run = 0 AND (o.id IN (SELECT order_id FROM bot_ledger_legs WHERE user_id = ?)
			OR (o.state IN ('placed', 'stopped', 'outcome_unknown', 'cashed_out') AND (o.state = 'outcome_unknown' OR o.flow_json LIKE ?)))
		ORDER BY o.id`, userID, userID, stuckFlow)
	return scanOrderRows(rows, err)
}

// BotOrdersWithPlacedLegs returns every non-dry order whose legs report a placed leg (the ledger's backfill), by id.
func (s *Service) BotOrdersWithPlacedLegs() ([]BotOrderRow, error) {
	rows, err := s.db.Query(`SELECT ` + botOrderColumns + ` FROM bot_orders o LEFT JOIN extension_codes c ON c.id = o.code_id
		WHERE o.dry_run = 0 AND o.legs_json LIKE '%"placed":true%' ORDER BY o.id`)
	return scanOrderRows(rows, err)
}

func scanOrderRows(rows *sql.Rows, err error) ([]BotOrderRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BotOrderRow{}
	for rows.Next() {
		r, err := scanBotOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
