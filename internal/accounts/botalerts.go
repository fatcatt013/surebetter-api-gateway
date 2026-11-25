package accounts

import (
	"database/sql"
	"errors"
	"strings"
)

// BotAlertRow is one bot alert (bot_alerts, migration 11). Times are Unix ms; ResolvedAt and DismissedAt are 0 until
// then. AgentCodeID 0, WindowID "" and OrderID "" = not about one.
type BotAlertRow struct {
	ID          string
	UserID      int64
	Kind        string // attention, window, agent or order
	Severity    string // attention, problem or info
	AgentCodeID int64
	WindowID    string
	OrderID     string
	Title       string
	Detail      string
	CreatedAt   int64
	ResolvedAt  int64
	DismissedAt int64
}

const botAlertCols = `id, user_id, kind, severity, agent_code_id, window_id, order_id, title, detail, created_at, resolved_at, dismissed_at`

func scanBotAlert(row interface{ Scan(...any) error }) (BotAlertRow, error) {
	var a BotAlertRow
	err := row.Scan(&a.ID, &a.UserID, &a.Kind, &a.Severity, &a.AgentCodeID, &a.WindowID, &a.OrderID, &a.Title, &a.Detail,
		&a.CreatedAt, &a.ResolvedAt, &a.DismissedAt)
	return a, err
}

// collectBotAlerts reads every row and closes rows.
func collectBotAlerts(rows *sql.Rows) ([]BotAlertRow, error) {
	defer rows.Close()
	out := []BotAlertRow{}
	for rows.Next() {
		a, err := scanBotAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertBotAlert stores a new alert.
func (s *Service) InsertBotAlert(a BotAlertRow) error {
	_, err := s.db.Exec(`INSERT INTO bot_alerts (`+botAlertCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, a.Kind, a.Severity, a.AgentCodeID, a.WindowID, a.OrderID, a.Title, a.Detail, a.CreatedAt, a.ResolvedAt, a.DismissedAt)
	return err
}

// ResolveBotAlert marks alert id resolved at at, unless it already is.
func (s *Service) ResolveBotAlert(id string, at int64) error {
	_, err := s.db.Exec(`UPDATE bot_alerts SET resolved_at = ? WHERE id = ? AND resolved_at = 0`, at, id)
	return err
}

// ListBotAlerts returns userID's alerts that are not dismissed: the open ones (not resolved) first, each part newest
// first, at most limit.
func (s *Service) ListBotAlerts(userID int64, limit int) ([]BotAlertRow, error) {
	rows, err := s.db.Query(`SELECT `+botAlertCols+` FROM bot_alerts WHERE user_id = ? AND dismissed_at = 0
		ORDER BY resolved_at <> 0, created_at DESC, id LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	return collectBotAlerts(rows)
}

// CountOpenBotAlerts counts userID's open attention and problem alerts (neither resolved nor dismissed).
func (s *Service) CountOpenBotAlerts(userID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM bot_alerts WHERE user_id = ? AND severity <> 'info' AND resolved_at = 0 AND dismissed_at = 0`,
		userID).Scan(&n)
	return n, err
}

// DismissBotAlert marks userID's alert id dismissed at at (a second call keeps the first time) and returns it; found
// is false when it is not one of userID's alerts.
func (s *Service) DismissBotAlert(userID int64, id string, at int64) (BotAlertRow, bool, error) {
	if _, err := s.db.Exec(`UPDATE bot_alerts SET dismissed_at = ? WHERE id = ? AND user_id = ? AND dismissed_at = 0`, at, id, userID); err != nil {
		return BotAlertRow{}, false, err
	}
	a, err := scanBotAlert(s.db.QueryRow(`SELECT `+botAlertCols+` FROM bot_alerts WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return BotAlertRow{}, false, nil
	}
	if err != nil {
		return BotAlertRow{}, false, err
	}
	return a, true, nil
}

// changeBotAlerts sets column (resolved_at or dismissed_at) to at on the alerts where selects and returns them changed,
// oldest first, in one transaction.
func (s *Service) changeBotAlerts(column, where string, at int64, args ...any) ([]BotAlertRow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+botAlertCols+` FROM bot_alerts WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	list, err := collectBotAlerts(rows)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE bot_alerts SET `+column+` = ? WHERE `+where, append([]any{at}, args...)...); err != nil {
		return nil, err
	}
	for i := range list {
		if column == "dismissed_at" {
			list[i].DismissedAt = at
		} else {
			list[i].ResolvedAt = at
		}
	}
	return list, tx.Commit()
}

// DismissAllBotAlerts dismisses every alert of userID that is not dismissed yet and returns them.
func (s *Service) DismissAllBotAlerts(userID, at int64) ([]BotAlertRow, error) {
	return s.changeBotAlerts("dismissed_at", `user_id = ? AND dismissed_at = 0`, at, userID)
}

// ResolveBotOrderAlerts resolves userID's open attention alerts about order orderID (the user marked it Checked) and
// returns them.
func (s *Service) ResolveBotOrderAlerts(userID int64, orderID string, at int64) ([]BotAlertRow, error) {
	return s.changeBotAlerts("resolved_at",
		`user_id = ? AND order_id = ? AND kind = 'attention' AND resolved_at = 0 AND dismissed_at = 0`, at, userID, orderID)
}

// ResolveBotAlertsTitled resolves userID's open (not resolved, not dismissed) alerts titled title (Resume of the auto
// pause: "Auto paused") and returns them.
func (s *Service) ResolveBotAlertsTitled(userID int64, title string, at int64) ([]BotAlertRow, error) {
	return s.changeBotAlerts("resolved_at", `user_id = ? AND title = ? AND resolved_at = 0 AND dismissed_at = 0`, at, userID, title)
}

// ResolveOpenBotAlerts resolves every open alert of kinds (at gateway start: the window and agent alerts the previous
// process can no longer resolve) and returns how many.
func (s *Service) ResolveOpenBotAlerts(kinds []string, at int64) (int64, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	args := []any{at}
	for _, k := range kinds {
		args = append(args, k)
	}
	res, err := s.db.Exec(`UPDATE bot_alerts SET resolved_at = ? WHERE resolved_at = 0 AND dismissed_at = 0 AND kind IN (?`+
		strings.Repeat(", ?", len(kinds)-1)+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneBotAlerts deletes every user's alerts created before beforeMs and returns how many.
func (s *Service) PruneBotAlerts(beforeMs int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM bot_alerts WHERE created_at < ?`, beforeMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BotAlertSettings returns userID's stored alert switches (a JSON object); found is false without a row.
func (s *Service) BotAlertSettings(userID int64) (string, bool, error) {
	var js string
	err := s.db.QueryRow(`SELECT json FROM bot_alert_settings WHERE user_id = ?`, userID).Scan(&js)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return js, true, nil
}

// SaveBotAlertSettings stores userID's alert switches (JSON) with updated_at = now.
func (s *Service) SaveBotAlertSettings(userID int64, js string) error {
	_, err := s.db.Exec(`INSERT INTO bot_alert_settings (user_id, json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET json = excluded.json, updated_at = excluded.updated_at`, userID, js, s.nowMs())
	return err
}

// OpenBotLoginAlerts returns every user's open login alerts (kind attention about a window and no order, neither
// resolved nor dismissed), oldest first: the gateway reads them back at start so they keep their scope.
func (s *Service) OpenBotLoginAlerts() ([]BotAlertRow, error) {
	rows, err := s.db.Query(`SELECT ` + botAlertCols + ` FROM bot_alerts WHERE kind = 'attention' AND window_id <> '' AND order_id = ''
		AND resolved_at = 0 AND dismissed_at = 0 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return collectBotAlerts(rows)
}
