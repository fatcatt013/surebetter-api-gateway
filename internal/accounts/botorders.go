package accounts

// BotOrderRow is one bot order in the history (bot_orders). Times are Unix ms; Stake is whole Kč.
type BotOrderRow struct {
	ID        string
	UserID    int64
	ArbKey    string
	CodeID    int64  // the agent it was sent to; 0 = never sent
	Agent     string // the agent's (code's) name; filled by reads only
	State     string // waiting, running, placed, stopped, outcome_unknown or cashed_out
	Reason    string
	DryRun    bool
	Stake     int64  // the ordered stakes summed until it ends; then the counted stake (day limit, history)
	InfoJSON  string // match, market, live, profitPct and the order fields the agent gets (market type, line …)
	LegsJSON  string // legs with windowId, last step and, once ended, what was really staked
	FlowJSON  string // the agent's last flow object ("" = none)
	CreatedAt int64
	SentAt    int64
	EndedAt   int64
}

// BotOrderDayStats sums a user's orders created in one day: all orders, the placed ones, and the
// live stake (dry_run = 0, every state: a final order's stake is what it counts, e.g. a stopped
// order's placed legs, normally 0).
type BotOrderDayStats struct {
	Orders    int
	Placed    int
	LiveStake int64
}

// InsertBotOrder stores a new order (Agent is ignored).
func (s *Service) InsertBotOrder(r BotOrderRow) error {
	_, err := s.db.Exec(`INSERT INTO bot_orders (id, user_id, arb_key, code_id, state, reason, dry_run, stake, info_json, legs_json,
			flow_json, created_at, sent_at, ended_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.UserID, r.ArbKey, r.CodeID, r.State, r.Reason, r.DryRun, r.Stake, r.InfoJSON, r.LegsJSON, r.FlowJSON, r.CreatedAt, r.SentAt, r.EndedAt)
	return err
}

// MarkBotOrderSent moves a waiting order to running: the agent, the mode it was sent in, its legs
// (now with window ids) and the send time. Other states are left alone.
func (s *Service) MarkBotOrderSent(id string, codeID int64, dryRun bool, legsJSON string, sentAt int64) error {
	_, err := s.db.Exec(`UPDATE bot_orders SET state = 'running', reason = '', code_id = ?, dry_run = ?, legs_json = ?, sent_at = ?
		WHERE id = ? AND state = 'waiting'`, codeID, dryRun, legsJSON, sentAt, id)
	return err
}

// SetBotOrderLegs replaces a running order's legs (a leg released by the agent); a waiting or ended
// order is left alone.
func (s *Service) SetBotOrderLegs(id, legsJSON string) error {
	_, err := s.db.Exec(`UPDATE bot_orders SET legs_json = ? WHERE id = ? AND state = 'running'`, legsJSON, id)
	return err
}

// EndBotOrder ends a waiting or running order with a final state (placed, stopped, outcome_unknown
// or cashed_out), its reason, its legs (with their last steps), its stake (the real total when the
// agent reported the legs) and the end time. An order that already ended is left alone.
func (s *Service) EndBotOrder(id, state, reason, legsJSON string, stake, endedAt int64) error {
	_, err := s.db.Exec(`UPDATE bot_orders SET state = ?, reason = ?, legs_json = ?, stake = ?, ended_at = ?
		WHERE id = ? AND state IN ('waiting', 'running')`, state, reason, legsJSON, stake, endedAt, id)
	return err
}

// SetBotOrderFlow stores a running order's last flow object. Other states are left alone.
func (s *Service) SetBotOrderFlow(id, flowJSON string) error {
	_, err := s.db.Exec(`UPDATE bot_orders SET flow_json = ? WHERE id = ? AND state = 'running'`, flowJSON, id)
	return err
}

const botOrderColumns = `o.id, o.user_id, o.arb_key, o.code_id, COALESCE(c.name, ''), o.state, o.reason, o.dry_run, o.stake,
	o.info_json, o.legs_json, o.flow_json, o.created_at, o.sent_at, o.ended_at`

// EndedBotOrders returns userID's orders created in [from, to) that ended (placed, stopped,
// outcome_unknown or cashed_out), newest first, at most limit.
func (s *Service) EndedBotOrders(userID, from, to int64, limit int) ([]BotOrderRow, error) {
	rows, err := s.db.Query(`SELECT `+botOrderColumns+` FROM bot_orders o LEFT JOIN extension_codes c ON c.id = o.code_id
		WHERE o.user_id = ? AND o.created_at >= ? AND o.created_at < ? AND o.state IN ('placed', 'stopped', 'outcome_unknown', 'cashed_out')
		ORDER BY o.created_at DESC, o.id DESC LIMIT ?`, userID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BotOrderRow{}
	for rows.Next() {
		var r BotOrderRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.ArbKey, &r.CodeID, &r.Agent, &r.State, &r.Reason, &r.DryRun, &r.Stake,
			&r.InfoJSON, &r.LegsJSON, &r.FlowJSON, &r.CreatedAt, &r.SentAt, &r.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BotOrderDay counts userID's orders created in [from, to) and sums their live stake.
func (s *Service) BotOrderDay(userID, from, to int64) (BotOrderDayStats, error) {
	var st BotOrderDayStats
	err := s.db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(state = 'placed'), 0),
			COALESCE(SUM(CASE WHEN dry_run = 0 THEN stake ELSE 0 END), 0)
		FROM bot_orders WHERE user_id = ? AND created_at >= ? AND created_at < ?`, userID, from, to).
		Scan(&st.Orders, &st.Placed, &st.LiveStake)
	return st, err
}

// CloseOpenBotOrders ends every order the last gateway process left open: waiting → stopped
// (a live one placed nothing: its stake becomes 0), running → outcome_unknown (its ordered stake
// stays), both with reason and ended_at. It returns the closed orders (with
// their new state), running ones first, each group by id.
func (s *Service) CloseOpenBotOrders(reason string, endedAt int64) ([]BotOrderRow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, user_id, arb_key, state FROM bot_orders WHERE state IN ('waiting', 'running')
		ORDER BY state = 'waiting', id`)
	if err != nil {
		return nil, err
	}
	var closed []BotOrderRow
	for rows.Next() {
		var r BotOrderRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.ArbKey, &r.State); err != nil {
			rows.Close()
			return nil, err
		}
		if r.State == "running" {
			r.State = "outcome_unknown"
		} else {
			r.State = "stopped"
		}
		r.Reason, r.EndedAt = reason, endedAt
		closed = append(closed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range closed {
		if _, err := tx.Exec(`UPDATE bot_orders SET state = ?, reason = ?, ended_at = ?,
				stake = CASE WHEN ? = 'stopped' AND dry_run = 0 THEN 0 ELSE stake END WHERE id = ?`, r.State, r.Reason, r.EndedAt, r.State, r.ID); err != nil {
			return nil, err
		}
	}
	return closed, tx.Commit()
}
