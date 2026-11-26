package accounts

import (
	"database/sql"
	"errors"
)

// BotLoginRow is one window's bookmaker login (bot_logins, migration 12). Secret is nonce ‖ AES-256-GCM of the user
// name and password, sealed by cmd/gateway with the key KeyID; this package never sees them in clear. UserTag is an
// HMAC of the lower-cased user name under that key. Times are Unix ms (0 = never); Failing is "", wrong_password or
// blocked; RevealsDay is the Prague day (YYYY-MM-DD) Reveals counts.
type BotLoginRow struct {
	UserID     int64
	CodeID     int64
	WindowID   string
	Bookmaker  string
	Secret     []byte
	KeyID      string
	UserTag    string
	UpdatedAt  int64
	LastOkAt   int64
	LastResult string
	LastAt     int64
	Failing    string
	RevealsDay string
	Reveals    int
}

// ErrNoLogin: no login is stored for that code and window.
var ErrNoLogin = errors.New("no_login")

// ErrStaleLogin: a result was for a login that has been saved again since (its updated_at differs).
var ErrStaleLogin = errors.New("stale_login")

const botLoginCols = `user_id, code_id, window_id, bookmaker, secret, key_id, user_tag, updated_at, last_ok_at, last_result, last_at,
	failing, reveals_day, reveals`

func scanBotLogin(row interface{ Scan(...any) error }) (BotLoginRow, error) {
	var l BotLoginRow
	err := row.Scan(&l.UserID, &l.CodeID, &l.WindowID, &l.Bookmaker, &l.Secret, &l.KeyID, &l.UserTag, &l.UpdatedAt, &l.LastOkAt,
		&l.LastResult, &l.LastAt, &l.Failing, &l.RevealsDay, &l.Reveals)
	return l, err
}

func collectBotLogins(rows *sql.Rows) ([]BotLoginRow, error) {
	defer rows.Close()
	out := []BotLoginRow{}
	for rows.Next() {
		l, err := scanBotLogin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SaveBotLogin inserts or replaces the login of (r.CodeID, r.WindowID): the new secret, key id, user tag and
// updated_at, failing cleared and the reveal count reset; the last result and last OK are kept. ErrNotFound (nothing
// stored) when r.CodeID is not a live code of r.UserID (revoked or deleted since the caller checked).
func (s *Service) SaveBotLogin(r BotLoginRow) error {
	res, err := s.db.Exec(`INSERT INTO bot_logins (user_id, code_id, window_id, bookmaker, secret, key_id, user_tag, updated_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM extension_codes WHERE id = ? AND user_id = ? AND revoked_at IS NULL)
		ON CONFLICT (code_id, window_id) DO UPDATE SET user_id = excluded.user_id, bookmaker = excluded.bookmaker,
			secret = excluded.secret, key_id = excluded.key_id, user_tag = excluded.user_tag, updated_at = excluded.updated_at,
			failing = '', reveals_day = '', reveals = 0`,
		r.UserID, r.CodeID, r.WindowID, r.Bookmaker, r.Secret, r.KeyID, r.UserTag, r.UpdatedAt, r.CodeID, r.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// BotLogin returns the login of codeID's window; found is false without one.
func (s *Service) BotLogin(codeID int64, windowID string) (BotLoginRow, bool, error) {
	l, err := scanBotLogin(s.db.QueryRow(`SELECT `+botLoginCols+` FROM bot_logins WHERE code_id = ? AND window_id = ?`, codeID, windowID))
	if errors.Is(err, sql.ErrNoRows) {
		return BotLoginRow{}, false, nil
	}
	return l, err == nil, err
}

// ListBotLogins returns userID's logins of bot codes that are not revoked, by code id then window id.
func (s *Service) ListBotLogins(userID int64) ([]BotLoginRow, error) {
	rows, err := s.db.Query(`SELECT `+prefixed("l.", botLoginCols)+` FROM bot_logins l
		JOIN extension_codes c ON c.id = l.code_id AND c.user_id = l.user_id AND c.revoked_at IS NULL
		WHERE l.user_id = ? ORDER BY l.code_id, l.window_id`, userID)
	if err != nil {
		return nil, err
	}
	return collectBotLogins(rows)
}

// ListBotLoginsOfCode returns codeID's logins by window id.
func (s *Service) ListBotLoginsOfCode(codeID int64) ([]BotLoginRow, error) {
	rows, err := s.db.Query(`SELECT `+botLoginCols+` FROM bot_logins WHERE code_id = ? ORDER BY window_id`, codeID)
	if err != nil {
		return nil, err
	}
	return collectBotLogins(rows)
}

// DeleteBotLogin deletes the login of codeID's window; false when there was none.
func (s *Service) DeleteBotLogin(codeID int64, windowID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM bot_logins WHERE code_id = ? AND window_id = ?`, codeID, windowID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetBotLoginResult stores a login attempt's result on codeID's window at at: last_result and last_at, and last_ok_at
// for ok. wrong_password and blocked also set failing on that row and on every row with the same user, code,
// bookmaker and user tag (they would fail the same way); spread is how many rows that set. One transaction. The
// returned row is the one before the change. ErrNoLogin without a row. With updatedAt set (the row's updated_at the
// agent's reveal gave it) and the row's no longer equal, nothing changes and ErrStaleLogin is returned with the row.
func (s *Service) SetBotLoginResult(codeID int64, windowID, result string, at int64, updatedAt *int64) (row BotLoginRow, spread int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return BotLoginRow{}, 0, err
	}
	defer tx.Rollback()
	row, err = scanBotLogin(tx.QueryRow(`SELECT `+botLoginCols+` FROM bot_logins WHERE code_id = ? AND window_id = ?`, codeID, windowID))
	if errors.Is(err, sql.ErrNoRows) {
		return BotLoginRow{}, 0, ErrNoLogin
	}
	if err != nil {
		return BotLoginRow{}, 0, err
	}
	if updatedAt != nil && *updatedAt != row.UpdatedAt {
		return row, 0, ErrStaleLogin
	}
	if _, err := tx.Exec(`UPDATE bot_logins SET last_result = ?, last_at = ?, last_ok_at = CASE WHEN ? = 'ok' THEN ? ELSE last_ok_at END
		WHERE code_id = ? AND window_id = ?`, result, at, result, at, codeID, windowID); err != nil {
		return BotLoginRow{}, 0, err
	}
	if result == "wrong_password" || result == "blocked" {
		res, err := tx.Exec(`UPDATE bot_logins SET failing = ? WHERE user_id = ? AND code_id = ? AND bookmaker = ? AND user_tag = ?`,
			result, row.UserID, codeID, row.Bookmaker, row.UserTag)
		if err != nil {
			return BotLoginRow{}, 0, err
		}
		spread, _ = res.RowsAffected()
	}
	return row, spread, tx.Commit()
}

// RevealBotLogin hands the login of codeID's window to use, with today's reveal count so far (0 when reveals_day is
// another day), inside one transaction: when use returns nil the count goes up by one and reveals_day becomes day;
// any error from use is returned and nothing changes. use must not touch the database. ErrNoLogin without a row.
func (s *Service) RevealBotLogin(codeID int64, windowID, day string, use func(row BotLoginRow, today int) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row, err := scanBotLogin(tx.QueryRow(`SELECT `+botLoginCols+` FROM bot_logins WHERE code_id = ? AND window_id = ?`, codeID, windowID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoLogin
	}
	if err != nil {
		return err
	}
	today := row.Reveals
	if row.RevealsDay != day {
		today = 0
	}
	if err := use(row, today); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE bot_logins SET reveals = ?, reveals_day = ? WHERE code_id = ? AND window_id = ?`,
		today+1, day, codeID, windowID); err != nil {
		return err
	}
	return tx.Commit()
}

// RekeyBotLogins seals every login of key fromKeyID again (key rotation at start), in one transaction: reseal returns
// a row's new secret, key id and user tag. A row reseal fails on is left as it is and counted in skipped.
func (s *Service) RekeyBotLogins(fromKeyID string, reseal func(BotLoginRow) (secret []byte, keyID, tag string, err error)) (done, skipped int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+botLoginCols+` FROM bot_logins WHERE key_id = ? ORDER BY code_id, window_id`, fromKeyID)
	if err != nil {
		return 0, 0, err
	}
	list, err := collectBotLogins(rows)
	if err != nil {
		return 0, 0, err
	}
	for _, l := range list {
		secret, keyID, tag, err := reseal(l)
		if err != nil {
			skipped++
			continue
		}
		if _, err := tx.Exec(`UPDATE bot_logins SET secret = ?, key_id = ?, user_tag = ? WHERE code_id = ? AND window_id = ?`,
			secret, keyID, tag, l.CodeID, l.WindowID); err != nil {
			return 0, 0, err
		}
		done++
	}
	return done, skipped, tx.Commit()
}

// prefixed puts p before each column of a comma-separated list.
func prefixed(p, cols string) string {
	out := []byte{}
	start := true
	for i := 0; i < len(cols); i++ {
		c := cols[i]
		if start && c != ' ' && c != '\t' && c != '\n' {
			out = append(out, p...)
			start = false
		}
		out = append(out, c)
		if c == ',' {
			start = true
		}
	}
	return string(out)
}
