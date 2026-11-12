package accounts

import (
	"encoding/json"
	"strings"
)

// BotLogLine is one validated log line an agent posted.
type BotLogLine struct {
	TS       int64
	Level    string // info, warn or error
	WindowID string
	OrderID  string
	Step     string
	Msg      string
	Detail   string // a JSON object, or "" for none
}

// BotLog is one stored log line as GET /api/me/bot/logs returns it.
type BotLog struct {
	ID       int64           `json:"id"`
	TS       int64           `json:"ts"`
	AgentID  int64           `json:"agentId"`
	Agent    string          `json:"agent"`
	Level    string          `json:"level"`
	WindowID string          `json:"windowId"`
	OrderID  string          `json:"orderId"`
	Step     string          `json:"step"`
	Msg      string          `json:"msg"`
	Detail   json.RawMessage `json:"detail,omitempty"`
}

// BotLogFilter selects a user's log lines. Zero fields do not filter.
type BotLogFilter struct {
	AgentID   int64  // code id
	WindowID  string // exact
	OrderID   string // exact
	Problems  bool   // warn and error only
	Query     string // substring of msg or step, case-insensitive (ASCII letters)
	After     int64  // ids above
	Before    int64  // ids below
	Ascending bool   // oldest first; default newest first
	Limit     int    // 0 = no limit
}

// AddBotLogs stores lines posted by codeID's agent, in one transaction.
func (s *Service) AddBotLogs(userID, codeID int64, lines []BotLogLine) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO bot_logs (user_id, code_id, ts, level, window_id, order_id, step, msg, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, l := range lines {
		if _, err := stmt.Exec(userID, codeID, l.TS, l.Level, l.WindowID, l.OrderID, l.Step, l.Msg, l.Detail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QueryBotLogs returns userID's log lines matching f, each with its agent's (code's) name.
func (s *Service) QueryBotLogs(userID int64, f BotLogFilter) ([]BotLog, error) {
	q := `SELECT l.id, l.ts, l.code_id, COALESCE(c.name, ''), l.level, l.window_id, l.order_id, l.step, l.msg, l.detail
		FROM bot_logs l LEFT JOIN extension_codes c ON c.id = l.code_id WHERE l.user_id = ?`
	args := []any{userID}
	if f.AgentID != 0 {
		q += ` AND l.code_id = ?`
		args = append(args, f.AgentID)
	}
	if f.WindowID != "" {
		q += ` AND l.window_id = ?`
		args = append(args, f.WindowID)
	}
	if f.OrderID != "" {
		q += ` AND l.order_id = ?`
		args = append(args, f.OrderID)
	}
	if f.Problems {
		q += ` AND l.level IN ('warn', 'error')`
	}
	if f.Query != "" {
		lq := strings.ToLower(f.Query)
		q += ` AND (instr(lower(l.msg), ?) > 0 OR instr(lower(l.step), ?) > 0)`
		args = append(args, lq, lq)
	}
	if f.After > 0 {
		q += ` AND l.id > ?`
		args = append(args, f.After)
	}
	if f.Before > 0 {
		q += ` AND l.id < ?`
		args = append(args, f.Before)
	}
	if f.Ascending {
		q += ` ORDER BY l.id`
	} else {
		q += ` ORDER BY l.id DESC`
	}
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BotLog{}
	for rows.Next() {
		var l BotLog
		var detail string
		if err := rows.Scan(&l.ID, &l.TS, &l.AgentID, &l.Agent, &l.Level, &l.WindowID, &l.OrderID, &l.Step, &l.Msg, &detail); err != nil {
			return nil, err
		}
		if detail != "" {
			l.Detail = json.RawMessage(detail)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// PruneBotLogs deletes every user's log lines with ts before beforeMs and returns how many.
func (s *Service) PruneBotLogs(beforeMs int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM bot_logs WHERE ts < ?`, beforeMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
