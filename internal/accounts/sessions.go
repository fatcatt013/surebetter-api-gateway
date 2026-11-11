package accounts

import "time"

const (
	// SessionTTL: a session ends this long after its last use.
	SessionTTL = 30 * 24 * time.Hour
	// touchEvery: last-seen and expiry are written at most this often per session or code.
	touchEvery = time.Minute
)

func (s *Service) createSession(q querier, userID int64) (token string, id int64, err error) {
	token = newSessionToken()
	now := s.nowMs()
	res, err := q.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hashSecret(token), userID, now, now, now+SessionTTL.Milliseconds())
	if err != nil {
		return "", 0, err
	}
	id, err = res.LastInsertId()
	return token, id, err
}
