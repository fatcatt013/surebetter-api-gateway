package accounts

import (
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

// NewExtCode is returned once, when a code is created.
type NewExtCode struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Code string `json:"code"`
	Hint string `json:"hint"`
}

// ExtCodeInfo is one row of a user's extension code list.
type ExtCodeInfo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Hint       string `json:"hint"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt *int64 `json:"last_used_at"`
	RevokedAt  *int64 `json:"revoked_at"`
}

func cleanName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" {
		n = "Extension"
	}
	if utf8.RuneCountInString(n) > 40 {
		return "", ErrBadName
	}
	return n, nil
}

// CreateExtensionCode makes a new code for userID's extension (one per browser or bot profile).
func (s *Service) CreateExtensionCode(userID int64, name string) (NewExtCode, error) {
	name, err := cleanName(name)
	if err != nil {
		return NewExtCode{}, err
	}
	code := newExtensionCode()
	res, err := s.db.Exec(`INSERT INTO extension_codes (user_id, name, code_hash, hint, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, name, hashSecret(code), hint(code), s.nowMs())
	if err != nil {
		return NewExtCode{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return NewExtCode{}, err
	}
	return NewExtCode{ID: id, Name: name, Code: code, Hint: hint(code)}, nil
}

// ListExtensionCodes returns userID's codes, newest first, revoked ones included.
func (s *Service) ListExtensionCodes(userID int64) ([]ExtCodeInfo, error) {
	rows, err := s.db.Query(`SELECT id, name, hint, created_at, last_used_at, revoked_at FROM extension_codes
		WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExtCodeInfo{}
	for rows.Next() {
		var c ExtCodeInfo
		var used, revoked sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Name, &c.Hint, &c.CreatedAt, &used, &revoked); err != nil {
			return nil, err
		}
		c.LastUsedAt, c.RevokedAt = nullable(used), nullable(revoked)
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeExtensionCode revokes one of userID's own codes and closes its sockets.
func (s *Service) RevokeExtensionCode(userID, id int64) error {
	res, err := s.db.Exec(`UPDATE extension_codes SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		s.nowMs(), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.conns.CloseCode(id)
	return nil
}

// AuthenticateCode returns the principal of a live extension code (pasted text is normalised).
// Revoked codes and disabled users do not authenticate.
func (s *Service) AuthenticateCode(code string) (Principal, bool, error) {
	code = normalizeCode(code)
	if !strings.HasPrefix(code, "SBX-") {
		return Principal{}, false, nil
	}
	var p Principal
	var lastUsed, onboarded, disabled sql.NullInt64
	err := s.db.QueryRow(`SELECT c.id, c.name, c.last_used_at, u.id, u.email, u.role, u.onboarded_at, u.disabled_at
		FROM extension_codes c JOIN users u ON u.id = c.user_id
		WHERE c.code_hash = ? AND c.revoked_at IS NULL`, hashSecret(code)).
		Scan(&p.CodeID, &p.CodeName, &lastUsed, &p.UserID, &p.Email, &p.Role, &onboarded, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, false, nil
	}
	if err != nil {
		return Principal{}, false, err
	}
	if disabled.Valid {
		return Principal{}, false, nil
	}
	now := s.nowMs()
	if !lastUsed.Valid || now-lastUsed.Int64 >= touchEvery.Milliseconds() {
		if _, err := s.db.Exec(`UPDATE extension_codes SET last_used_at = ? WHERE id = ?`, now, p.CodeID); err != nil {
			return Principal{}, false, err
		}
	}
	p.Onboarded = onboarded.Valid
	return p, true, nil
}
