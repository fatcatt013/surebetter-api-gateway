package accounts

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

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

// Login checks email + password and starts a session. Five failures from one IP, or for one
// email, lock it for 15 minutes (LockedError).
func (s *Service) Login(ip, email, password string) (Principal, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if err := s.checkIP(ip); err != nil {
		return Principal{}, "", err
	}
	if ok, wait := s.emailLimit.check(email); !ok {
		return Principal{}, "", &LockedError{RetryAfter: wait}
	}
	var p Principal
	var hash string
	var onboarded, disabled sql.NullInt64
	err := s.db.QueryRow(`SELECT id, email, role, password_hash, onboarded_at, disabled_at FROM users WHERE email = ?`, email).
		Scan(&p.UserID, &p.Email, &p.Role, &hash, &onboarded, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		verifyPassword(password, dummyHash())
		s.ipLimit.fail(ip)
		s.emailLimit.fail(email)
		return Principal{}, "", ErrInvalidCredentials
	}
	if err != nil {
		return Principal{}, "", err
	}
	if !verifyPassword(password, hash) {
		s.ipLimit.fail(ip)
		s.emailLimit.fail(email)
		return Principal{}, "", ErrInvalidCredentials
	}
	if disabled.Valid {
		return Principal{}, "", ErrDisabled
	}
	s.emailLimit.reset(email)
	token, sid, err := s.createSession(s.db, p.UserID)
	if err != nil {
		return Principal{}, "", err
	}
	p.SessionID = sid
	p.Onboarded = onboarded.Valid
	return p, token, nil
}

// Logout ends a session and closes its sockets.
func (s *Service) Logout(sessionID int64) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		return err
	}
	s.conns.CloseSession(sessionID)
	return nil
}

// AuthenticateSession returns the principal of a live session token and extends the session.
// Expired sessions and disabled users do not authenticate.
func (s *Service) AuthenticateSession(token string) (Principal, bool, error) {
	if token == "" {
		return Principal{}, false, nil
	}
	var p Principal
	var lastSeen, expires int64
	var onboarded, disabled sql.NullInt64
	err := s.db.QueryRow(`SELECT s.id, s.last_seen_at, s.expires_at, u.id, u.email, u.role, u.onboarded_at, u.disabled_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, hashSecret(token)).
		Scan(&p.SessionID, &lastSeen, &expires, &p.UserID, &p.Email, &p.Role, &onboarded, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, false, nil
	}
	if err != nil {
		return Principal{}, false, err
	}
	now := s.nowMs()
	if now >= expires {
		_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, p.SessionID)
		return Principal{}, false, err
	}
	if disabled.Valid {
		return Principal{}, false, nil
	}
	if now-lastSeen >= touchEvery.Milliseconds() {
		if _, err := s.db.Exec(`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
			now, now+SessionTTL.Milliseconds(), p.SessionID); err != nil {
			return Principal{}, false, err
		}
		if _, err := s.db.Exec(`UPDATE users SET last_seen_at = ? WHERE id = ?`, now, p.UserID); err != nil {
			return Principal{}, false, err
		}
	}
	p.Onboarded = onboarded.Valid
	return p, true, nil
}
