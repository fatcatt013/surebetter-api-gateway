package accounts

import (
	"database/sql"
	"errors"
	"time"
)

type InviteStatus string

const (
	InviteValid   InviteStatus = "valid"
	InviteExpired InviteStatus = "expired"
	InviteUsed    InviteStatus = "used"
	InviteRevoked InviteStatus = "revoked"
	InviteUnknown InviteStatus = "unknown"

	InviteTTL = 14 * 24 * time.Hour
)

// NewInvite is returned once, when an invite is created: the code is never stored in clear.
type NewInvite struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	Path      string `json:"path"` // the frontend prefixes location.origin
	ExpiresAt int64  `json:"expires_at"`
}

// InviteInfo is one row of the admin invite list.
type InviteInfo struct {
	ID        int64        `json:"id"`
	Hint      string       `json:"hint"`
	Role      string       `json:"role"`
	Status    InviteStatus `json:"status"`
	CreatedAt int64        `json:"created_at"`
	ExpiresAt int64        `json:"expires_at"`
	UsedBy    *string      `json:"used_by"`
}

type inviteRow struct {
	id   int64
	role string
}

func inviteStatus(now, expiresAt int64, usedAt, revokedAt sql.NullInt64) InviteStatus {
	switch {
	case revokedAt.Valid:
		return InviteRevoked
	case usedAt.Valid:
		return InviteUsed
	case now >= expiresAt:
		return InviteExpired
	}
	return InviteValid
}

func (s *Service) inviteByCode(q querier, code string) (inviteRow, InviteStatus, error) {
	var r inviteRow
	var exp int64
	var usedAt, revokedAt sql.NullInt64
	err := q.QueryRow(`SELECT id, role, expires_at, used_at, revoked_at FROM invites WHERE code_hash = ?`,
		hashSecret(normalizeCode(code))).Scan(&r.id, &r.role, &exp, &usedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, InviteUnknown, nil
	}
	if err != nil {
		return r, "", err
	}
	return r, inviteStatus(s.nowMs(), exp, usedAt, revokedAt), nil
}

// CheckInvite reports an invite code's status (onboarding step 1). Unknown codes count as
// failures for ip, so codes cannot be guessed.
func (s *Service) CheckInvite(ip, code string) (InviteStatus, error) {
	if err := s.checkIP(ip); err != nil {
		return "", err
	}
	_, st, err := s.inviteByCode(s.db, code)
	if err == nil && st == InviteUnknown {
		s.ipLimit.fail(ip)
	}
	return st, err
}

// CreateInvite makes an invite for role (empty = user). createdBy 0 = no creator.
func (s *Service) CreateInvite(createdBy int64, role string) (NewInvite, error) {
	if role == "" {
		role = RoleUser
	}
	if !validRole(role) {
		return NewInvite{}, ErrBadRole
	}
	return s.insertInvite(s.db, createdBy, role, false)
}

func (s *Service) insertInvite(q querier, createdBy int64, role string, bootstrap bool) (NewInvite, error) {
	code := newShortCode()
	now := s.nowMs()
	exp := now + InviteTTL.Milliseconds()
	res, err := q.Exec(`INSERT INTO invites (code_hash, hint, role, created_by, created_at, expires_at, bootstrap)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, hashSecret(code), hint(code), role, nullID(createdBy), now, exp, bootstrap)
	if err != nil {
		return NewInvite{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return NewInvite{}, err
	}
	return NewInvite{ID: id, Code: code, Path: "/app/invite?code=" + code, ExpiresAt: exp}, nil
}

// ListInvites returns every invite, newest first.
func (s *Service) ListInvites() ([]InviteInfo, error) {
	rows, err := s.db.Query(`SELECT i.id, i.hint, i.role, i.created_at, i.expires_at, i.used_at, i.revoked_at, u.email
		FROM invites i LEFT JOIN users u ON u.id = i.used_by ORDER BY i.created_at DESC, i.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.nowMs()
	out := []InviteInfo{}
	for rows.Next() {
		var in InviteInfo
		var usedAt, revokedAt sql.NullInt64
		var usedBy sql.NullString
		if err := rows.Scan(&in.ID, &in.Hint, &in.Role, &in.CreatedAt, &in.ExpiresAt, &usedAt, &revokedAt, &usedBy); err != nil {
			return nil, err
		}
		in.Status = inviteStatus(now, in.ExpiresAt, usedAt, revokedAt)
		if usedBy.Valid {
			e := usedBy.String
			in.UsedBy = &e
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// RevokeInvite revokes an unused, unrevoked invite. ErrNotFound otherwise.
func (s *Service) RevokeInvite(id int64) error {
	res, err := s.db.Exec(`UPDATE invites SET revoked_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL`, s.nowMs(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// EnsureBootstrapInvite: while no user exists, revokes earlier unused bootstrap invites and
// creates a fresh admin invite (created = true). Once a user exists it does nothing.
func (s *Service) EnsureBootstrapInvite() (NewInvite, bool, error) {
	var users int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		return NewInvite{}, false, err
	}
	if users > 0 {
		return NewInvite{}, false, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return NewInvite{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE invites SET revoked_at = ? WHERE bootstrap = 1 AND used_at IS NULL AND revoked_at IS NULL`, s.nowMs()); err != nil {
		return NewInvite{}, false, err
	}
	inv, err := s.insertInvite(tx, 0, RoleAdmin, true)
	if err != nil {
		return NewInvite{}, false, err
	}
	return inv, true, tx.Commit()
}
