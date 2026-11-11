package accounts

import (
	"database/sql"
	"errors"
	"time"
)

const ResetTTL = 24 * time.Hour

// NewReset is returned once, when an admin issues a password reset.
type NewReset struct {
	Code      string `json:"code"`
	Path      string `json:"path"`
	ExpiresAt int64  `json:"expires_at"`
}

// CreateReset issues a one-time password reset code for userID.
func (s *Service) CreateReset(adminID, userID int64) (NewReset, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&n); err != nil {
		return NewReset{}, err
	}
	if n == 0 {
		return NewReset{}, ErrNotFound
	}
	code := newShortCode()
	now := s.nowMs()
	exp := now + ResetTTL.Milliseconds()
	if _, err := s.db.Exec(`INSERT INTO reset_codes (user_id, code_hash, hint, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, hashSecret(code), hint(code), adminID, now, exp); err != nil {
		return NewReset{}, err
	}
	return NewReset{Code: code, Path: "/app/reset?code=" + code, ExpiresAt: exp}, nil
}

// resetByCode finds a reset code; usable = not used, not expired, and the user not disabled.
func (s *Service) resetByCode(q querier, code string) (id, userID int64, email string, usable bool, err error) {
	var exp int64
	var used, disabled sql.NullInt64
	err = q.QueryRow(`SELECT r.id, r.user_id, u.email, r.expires_at, r.used_at, u.disabled_at FROM reset_codes r
		JOIN users u ON u.id = r.user_id WHERE r.code_hash = ?`, hashSecret(normalizeCode(code))).
		Scan(&id, &userID, &email, &exp, &used, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, "", false, nil
	}
	if err != nil {
		return 0, 0, "", false, err
	}
	return id, userID, email, !used.Valid && !disabled.Valid && s.nowMs() < exp, nil
}

// CheckReset returns the email a usable reset code belongs to (for the "New password" page).
// Unusable codes count as failures for ip.
func (s *Service) CheckReset(ip, code string) (string, bool, error) {
	if err := s.checkIP(ip); err != nil {
		return "", false, err
	}
	_, _, email, ok, err := s.resetByCode(s.db, code)
	if err != nil {
		return "", false, err
	}
	if !ok {
		s.ipLimit.fail(ip)
		return "", false, nil
	}
	return email, true, nil
}

// ResetPassword sets a new password from a reset code, ends the user's sessions, closes their
// sockets and starts a new session.
func (s *Service) ResetPassword(ip, code, password string) (Principal, string, error) {
	if err := s.checkIP(ip); err != nil {
		return Principal{}, "", err
	}
	if err := checkPassword(password); err != nil {
		return Principal{}, "", err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return Principal{}, "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Principal{}, "", err
	}
	defer tx.Rollback()
	rid, uid, _, ok, err := s.resetByCode(tx, code)
	if err != nil {
		return Principal{}, "", err
	}
	if !ok {
		s.ipLimit.fail(ip)
		return Principal{}, "", ErrResetInvalid
	}
	var p Principal
	var onboarded, disabled sql.NullInt64
	if err := tx.QueryRow(`SELECT id, email, role, onboarded_at, disabled_at FROM users WHERE id = ?`, uid).
		Scan(&p.UserID, &p.Email, &p.Role, &onboarded, &disabled); err != nil {
		return Principal{}, "", err
	}
	if disabled.Valid {
		return Principal{}, "", ErrDisabled
	}
	now := s.nowMs()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE users SET password_hash = ? WHERE id = ?`, []any{hash, uid}},
		{`UPDATE reset_codes SET used_at = ? WHERE id = ?`, []any{now, rid}},
		{`DELETE FROM sessions WHERE user_id = ?`, []any{uid}},
	} {
		if _, err := tx.Exec(q.sql, q.args...); err != nil {
			return Principal{}, "", err
		}
	}
	token, sid, err := s.createSession(tx, uid)
	if err != nil {
		return Principal{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Principal{}, "", err
	}
	s.conns.CloseUser(uid) // the new session has no sockets yet; extension sockets reconnect with a ticket
	p.SessionID = sid
	p.Onboarded = onboarded.Valid
	return p, token, nil
}
