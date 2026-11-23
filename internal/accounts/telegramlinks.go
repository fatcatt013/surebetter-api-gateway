package accounts

import (
	"database/sql"
	"errors"
)

// TelegramLink is a user's private Telegram chat for bot alerts (telegram_links, migration 11).
type TelegramLink struct {
	ChatID   int64
	LinkedAt int64
}

// SetTelegramLink links userID's alerts to chatID (replacing an older link), linked now.
func (s *Service) SetTelegramLink(userID, chatID int64) error {
	_, err := s.db.Exec(`INSERT INTO telegram_links (user_id, chat_id, linked_at) VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET chat_id = excluded.chat_id, linked_at = excluded.linked_at`,
		userID, chatID, s.nowMs())
	return err
}

// TelegramLink returns userID's link; ok is false when it has none.
func (s *Service) TelegramLink(userID int64) (TelegramLink, bool, error) {
	var l TelegramLink
	err := s.db.QueryRow(`SELECT chat_id, linked_at FROM telegram_links WHERE user_id = ?`, userID).Scan(&l.ChatID, &l.LinkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TelegramLink{}, false, nil
	}
	if err != nil {
		return TelegramLink{}, false, err
	}
	return l, true, nil
}

// DeleteTelegramLink removes userID's link (none is fine).
func (s *Service) DeleteTelegramLink(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM telegram_links WHERE user_id = ?`, userID)
	return err
}

// DeleteTelegramLinkIfChat removes userID's link only while it still points at chatID (a refused message to an older
// chat leaves a newer link alone); removed reports whether it did.
func (s *Service) DeleteTelegramLinkIfChat(userID, chatID int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM telegram_links WHERE user_id = ? AND chat_id = ?`, userID, chatID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
