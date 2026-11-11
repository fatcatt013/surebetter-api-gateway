package accounts

import "encoding/json"

// MaxStateBytes caps one state value.
const MaxStateBytes = 256 << 10

// stateKeys are the per-user settings stored on the account (spec §2). The light/dark theme is
// deliberately absent: it stays in the browser.
var stateKeys = map[string]bool{
	"filters.last":  true,
	"presets":       true,
	"bankroll":      true,
	"hidden":        true,
	"pins":          true,
	"rules":         true,
	"alerts.readAt": true,
	"alerts.muted":  true,
	"view":          true,
	"value.floor":   true,
	"value.sort":    true,
	"value.sortDir": true,
	"value.odds":    true,
	"value.refs":    true,
	"help.chapter":  true,
	"profile":       true,
}

// GetState returns all of a user's state keys with their raw JSON values.
func (s *Service) GetState(userID int64) (map[string]json.RawMessage, error) {
	rows, err := s.db.Query(`SELECT key, value FROM user_state WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}

// PutState saves one key (last write wins) and notifies the user's other open sockets.
func (s *Service) PutState(p Principal, key string, value json.RawMessage) error {
	if !stateKeys[key] {
		return ErrUnknownKey
	}
	if len(value) > MaxStateBytes {
		return ErrTooLarge
	}
	if len(value) == 0 || !json.Valid(value) {
		return ErrBadValue
	}
	if _, err := s.db.Exec(`INSERT INTO user_state (user_id, key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		p.UserID, key, string(value), s.nowMs()); err != nil {
		return err
	}
	s.conns.NotifyState(p.UserID, key, p.SessionID)
	return nil
}

// SetOnboarded marks onboarding finished (idempotent: the first time is kept).
func (s *Service) SetOnboarded(userID int64) error {
	_, err := s.db.Exec(`UPDATE users SET onboarded_at = COALESCE(onboarded_at, ?) WHERE id = ?`, s.nowMs(), userID)
	return err
}
