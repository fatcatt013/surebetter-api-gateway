package accounts

import "strings"

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
