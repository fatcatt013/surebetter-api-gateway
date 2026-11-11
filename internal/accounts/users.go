package accounts

import (
	"database/sql"
	"strings"
)

// normalizeEmail lower-cases and trims; it rejects anything that is not one local@domain.tld.
func normalizeEmail(s string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(s))
	at := strings.IndexByte(e, '@')
	if len(e) > 254 || at < 1 || at != strings.LastIndexByte(e, '@') || strings.ContainsAny(e, " \t\r\n") {
		return "", ErrInvalidEmail
	}
	domain := e[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", ErrInvalidEmail
	}
	return e, nil
}

// Register uses up an invite and creates its user with the invite's role, signed in.
// ip is rate-limited: unusable invite codes count as failures.
func (s *Service) Register(ip, code, email, password string) (Principal, string, error) {
	if err := s.checkIP(ip); err != nil {
		return Principal{}, "", err
	}
	email, err := normalizeEmail(email)
	if err != nil {
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
	inv, status, err := s.inviteByCode(tx, code)
	if err != nil {
		return Principal{}, "", err
	}
	if status != InviteValid {
		s.ipLimit.fail(ip)
		return Principal{}, "", &InviteError{Status: status}
	}
	now := s.nowMs()
	res, err := tx.Exec(`INSERT INTO users (email, password_hash, role, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		email, hash, inv.role, now, now)
	if isUnique(err) {
		return Principal{}, "", ErrEmailTaken
	}
	if err != nil {
		return Principal{}, "", err
	}
	uid, err := res.LastInsertId()
	if err != nil {
		return Principal{}, "", err
	}
	if _, err := tx.Exec(`UPDATE invites SET used_by = ?, used_at = ? WHERE id = ?`, uid, now, inv.id); err != nil {
		return Principal{}, "", err
	}
	token, sid, err := s.createSession(tx, uid)
	if err != nil {
		return Principal{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Principal{}, "", err
	}
	return Principal{UserID: uid, Email: email, Role: inv.role, SessionID: sid}, token, nil
}

// UserInfo is one row of the admin user list.
type UserInfo struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	CreatedAt   int64  `json:"created_at"`
	OnboardedAt *int64 `json:"onboarded_at"`
	LastSeenAt  *int64 `json:"last_seen_at"`
	DisabledAt  *int64 `json:"disabled_at"`
}

// ListUsers returns every user, oldest first.
func (s *Service) ListUsers() ([]UserInfo, error) {
	rows, err := s.db.Query(`SELECT id, email, role, created_at, onboarded_at, last_seen_at, disabled_at FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserInfo{}
	for rows.Next() {
		var u UserInfo
		var onboarded, seen, disabled sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.CreatedAt, &onboarded, &seen, &disabled); err != nil {
			return nil, err
		}
		u.OnboardedAt, u.LastSeenAt, u.DisabledAt = nullable(onboarded), nullable(seen), nullable(disabled)
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetDisabled disables or re-enables a user. Disabling ends their sessions and closes their
// sockets; their extension codes stop authenticating while disabled. Admins cannot disable themselves.
func (s *Service) SetDisabled(adminID, userID int64, disabled bool) error {
	if adminID == userID {
		return ErrSelf
	}
	var at any // nil = NULL (enabled)
	if disabled {
		at = s.nowMs()
	}
	res, err := s.db.Exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, at, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if disabled {
		if _, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return err
		}
		s.conns.CloseUser(userID)
	}
	return nil
}
