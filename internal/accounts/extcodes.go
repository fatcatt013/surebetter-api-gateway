package accounts

import (
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog/log"
)

// Code kinds. A bot code can do everything an extension code can (read-only), plus open the bot
// socket (/ws?bot=1) and post bot status and events.
const (
	KindExtension = "extension"
	KindBot       = "bot"
)

// NewExtCode is returned once, when a code is created.
type NewExtCode struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Code string `json:"code"`
	Hint string `json:"hint"`
}

// ExtCodeInfo is one row of a user's extension code list.
type ExtCodeInfo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Hint       string `json:"hint"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt *int64 `json:"last_used_at"`
	RevokedAt  *int64 `json:"revoked_at"`
	Viewable   bool   `json:"viewable"` // not revoked, sealed, and it opens with a known key
}

// CodeCipher seals a new code for its row and opens it again (cmd/gateway's, under a key derived from BOT_LOGINS_KEY;
// the row's user and code ids are bound into the sealed value). Errors never hold the code.
type CodeCipher interface {
	SealCode(userID, codeID int64, code string) ([]byte, error)
	OpenCode(userID, codeID int64, sealed []byte) (string, error)
}

// SetCodeCipher sets the cipher of new codes and of GET /api/me/extension-codes/{id}/code (nil = codes are not sealed:
// none is viewable), and whether that route also answers over plain HTTP (BOT_LOGINS_ALLOW_HTTP=1). Call once at start.
func (s *Service) SetCodeCipher(c CodeCipher, allowHTTP bool) {
	s.codes, s.codesHTTP = c, allowHTTP
}

// openCode is the code sealed in enc for (userID, id); false when there is no cipher, nothing sealed, or it does not open.
func (s *Service) openCode(userID, id int64, enc []byte) (string, bool) {
	if s.codes == nil || len(enc) == 0 {
		return "", false
	}
	code, err := s.codes.OpenCode(userID, id, enc)
	return code, err == nil
}

func cleanName(n, fallback string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" {
		n = fallback
	}
	if utf8.RuneCountInString(n) > 40 {
		return "", ErrBadName
	}
	return n, nil
}

// CreateExtensionCode makes a new extension-kind code for userID (one per browser profile).
func (s *Service) CreateExtensionCode(userID int64, name string) (NewExtCode, error) {
	return s.CreateCode(userID, name, KindExtension)
}

// CreateCode makes a new code of kind ("" or "extension", or "bot") for userID. An empty name
// becomes "Extension" or "Bot".
func (s *Service) CreateCode(userID int64, name, kind string) (NewExtCode, error) {
	fallback := "Extension"
	switch kind {
	case "", KindExtension:
		kind = KindExtension
	case KindBot:
		fallback = "Bot"
	default:
		return NewExtCode{}, ErrBadKind
	}
	name, err := cleanName(name, fallback)
	if err != nil {
		return NewExtCode{}, err
	}
	code := newExtensionCode()
	tx, err := s.db.Begin()
	if err != nil {
		return NewExtCode{}, err
	}
	defer tx.Rollback()
	// The id is above every id in use and every id ever deleted (extension_code_ids), never one a deleted code had.
	res, err := tx.Exec(`INSERT INTO extension_codes (id, user_id, name, kind, code_hash, hint, created_at)
		VALUES (MAX(COALESCE((SELECT MAX(id) FROM extension_codes), 0), COALESCE((SELECT max_deleted FROM extension_code_ids WHERE id = 1), 0)) + 1,
			?, ?, ?, ?, ?, ?)`,
		userID, name, kind, hashSecret(code), hint(code), s.nowMs())
	if err != nil {
		return NewExtCode{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return NewExtCode{}, err
	}
	// Sealed so it can be shown again; without a key, or when sealing fails, it is created all the same (not viewable).
	if s.codes != nil {
		enc, err := s.codes.SealCode(userID, id, code)
		if err != nil {
			log.Error().Int64("user", userID).Int64("code", id).Msg("extension code not sealed: it cannot be shown again")
		} else if _, err := tx.Exec(`UPDATE extension_codes SET code_enc = ? WHERE id = ?`, enc, id); err != nil {
			return NewExtCode{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return NewExtCode{}, err
	}
	return NewExtCode{ID: id, Name: name, Kind: kind, Code: code, Hint: hint(code)}, nil
}

// ListExtensionCodes returns userID's codes (both kinds), newest first, revoked ones included.
func (s *Service) ListExtensionCodes(userID int64) ([]ExtCodeInfo, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, hint, created_at, last_used_at, revoked_at, code_enc FROM extension_codes
		WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExtCodeInfo{}
	for rows.Next() {
		var c ExtCodeInfo
		var used, revoked sql.NullInt64
		var enc []byte
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Hint, &c.CreatedAt, &used, &revoked, &enc); err != nil {
			return nil, err
		}
		c.LastUsedAt, c.RevokedAt = nullable(used), nullable(revoked)
		if !revoked.Valid {
			_, c.Viewable = s.openCode(userID, c.ID, enc)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevealExtensionCode returns one of userID's own codes in clear: ErrNotFound (none of theirs), ErrCodeRevoked, or
// ErrNotViewable (not sealed: older than migration 14 or made without a key; or it does not open with a known key).
func (s *Service) RevealExtensionCode(userID, id int64) (string, error) {
	var revoked sql.NullInt64
	var enc []byte
	err := s.db.QueryRow(`SELECT revoked_at, code_enc FROM extension_codes WHERE id = ? AND user_id = ?`, id, userID).Scan(&revoked, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if revoked.Valid {
		return "", ErrCodeRevoked
	}
	code, ok := s.openCode(userID, id, enc)
	if !ok {
		return "", ErrNotViewable
	}
	return code, nil
}

// SealedCodes returns every live sealed code as (user id, code id, sealed), for re-sealing after a key rotation.
func (s *Service) SealedCodes() ([]SealedCode, error) {
	rows, err := s.db.Query(`SELECT user_id, id, code_enc FROM extension_codes WHERE code_enc IS NOT NULL AND revoked_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SealedCode
	for rows.Next() {
		var c SealedCode
		if err := rows.Scan(&c.UserID, &c.CodeID, &c.Sealed); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SealedCode is one live sealed code (SealedCodes).
type SealedCode struct {
	UserID, CodeID int64
	Sealed         []byte
}

// ResealCodes re-seals live codes in one transaction: reseal returns the new sealed value, or ok false to leave a
// row as it is. done is how many rows changed.
func (s *Service) ResealCodes(reseal func(SealedCode) (sealed []byte, ok bool)) (done int, err error) {
	list, err := s.SealedCodes()
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, c := range list {
		enc, ok := reseal(c)
		if !ok {
			continue
		}
		res, err := tx.Exec(`UPDATE extension_codes SET code_enc = ? WHERE id = ? AND code_enc = ?`, enc, c.CodeID, c.Sealed)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			done++
		}
	}
	return done, tx.Commit()
}

// RevokeExtensionCode revokes one of userID's own codes, forgets its sealed copy (it can no longer be shown) and deletes
// its agent's bookmaker logins (one transaction), then closes its sockets.
func (s *Service) RevokeExtensionCode(userID, id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE extension_codes SET revoked_at = ?, code_enc = NULL WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		s.nowMs(), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM bot_logins WHERE code_id = ?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.conns.CloseCode(id)
	return nil
}

// DeleteExtensionCode deletes one of userID's own codes for good, revoked or not (ErrNotFound when none of theirs).
// One transaction: its agent's bookmaker logins, recordings and agent row (bot_agents and bot_recordings reference the
// code), the code's row, and the id raised into extension_code_ids so it is never handed out again. Then, as a revoke,
// its sockets close (the bot alerts forget the agent first). Its orders, logs and alerts stay (their code_id is kept;
// the agent's name reads empty from then on).
func (s *Service) DeleteExtensionCode(userID, id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRow(`SELECT 1 FROM extension_codes WHERE id = ? AND user_id = ?`, id, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM bot_logins WHERE code_id = ?`,
		`DELETE FROM bot_recordings WHERE code_id = ?`,
		`DELETE FROM bot_agents WHERE code_id = ?`,
		`DELETE FROM extension_codes WHERE id = ?`,
		`UPDATE extension_code_ids SET max_deleted = MAX(max_deleted, ?1) WHERE id = 1`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
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
	err := s.db.QueryRow(`SELECT c.id, c.name, c.kind, c.last_used_at, u.id, u.email, u.role, u.onboarded_at, u.disabled_at
		FROM extension_codes c JOIN users u ON u.id = c.user_id
		WHERE c.code_hash = ? AND c.revoked_at IS NULL`, hashSecret(code)).
		Scan(&p.CodeID, &p.CodeName, &p.CodeKind, &lastUsed, &p.UserID, &p.Email, &p.Role, &onboarded, &disabled)
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
