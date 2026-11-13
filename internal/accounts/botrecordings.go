package accounts

import (
	"database/sql"
	"errors"
)

// BotRecordingInfo is one agent's stored recording's summary: paths (mouse) or sessions (typing), and when it was uploaded.
type BotRecordingInfo struct {
	Count     int
	UpdatedAt int64
}

// SaveBotRecording replaces agent codeID's (userID's) recording of kind (mouse or typing) and returns
// its updated_at (now).
func (s *Service) SaveBotRecording(userID, codeID int64, kind, bodyJSON string, count int) (int64, error) {
	now := s.nowMs()
	_, err := s.db.Exec(`INSERT INTO bot_recordings (code_id, user_id, kind, body_json, count, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (code_id, kind) DO UPDATE SET body_json = excluded.body_json, count = excluded.count, updated_at = excluded.updated_at`,
		codeID, userID, kind, bodyJSON, count, now)
	return now, err
}

// BotRecordings returns agent codeID's recordings by kind (only the kinds it has).
func (s *Service) BotRecordings(codeID int64) (map[string]BotRecordingInfo, error) {
	rows, err := s.db.Query(`SELECT kind, count, updated_at FROM bot_recordings WHERE code_id = ?`, codeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]BotRecordingInfo{}
	for rows.Next() {
		var kind string
		var r BotRecordingInfo
		if err := rows.Scan(&kind, &r.Count, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out[kind] = r
	}
	return out, rows.Err()
}

// BotRecording returns agent codeID's recording of kind; found is false when there is none.
func (s *Service) BotRecording(codeID int64, kind string) (body string, updatedAt int64, found bool, err error) {
	err = s.db.QueryRow(`SELECT body_json, updated_at FROM bot_recordings WHERE code_id = ? AND kind = ?`, codeID, kind).Scan(&body, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return body, updatedAt, err == nil, err
}
