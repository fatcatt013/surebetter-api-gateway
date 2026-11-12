package accounts

// BotAgentStatus is what an agent's status post stores on its registry row.
type BotAgentStatus struct {
	Version     string
	HostName    string
	WindowsJSON string // a JSON array of window objects; "" is stored as "[]"
}

// BotAgentRow is one agent of a user: a non-revoked bot code that has connected at least once.
type BotAgentRow struct {
	CodeID int64
	Name   string // the code's name
	BotAgentStatus
	FirstSeenAt int64
	LastSeenAt  int64
}

// TouchBotAgent records that codeID's agent is connected (or just disconnected): it inserts the
// agent's row (first and last seen now) or moves its last seen to now.
func (s *Service) TouchBotAgent(userID, codeID int64) error {
	now := s.nowMs()
	_, err := s.db.Exec(`INSERT INTO bot_agents (code_id, user_id, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (code_id) DO UPDATE SET last_seen_at = excluded.last_seen_at`, codeID, userID, now, now)
	return err
}

// SaveBotAgentStatus stores an agent's version, host name and windows and moves its last seen to now
// (inserting the row if the agent has none yet).
func (s *Service) SaveBotAgentStatus(userID, codeID int64, st BotAgentStatus) error {
	if st.WindowsJSON == "" {
		st.WindowsJSON = "[]"
	}
	now := s.nowMs()
	_, err := s.db.Exec(`INSERT INTO bot_agents (code_id, user_id, version, host_name, windows_json, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (code_id) DO UPDATE SET version = excluded.version, host_name = excluded.host_name,
			windows_json = excluded.windows_json, last_seen_at = excluded.last_seen_at`,
		codeID, userID, st.Version, st.HostName, st.WindowsJSON, now, now)
	return err
}

// ListBotAgents returns userID's agents: its non-revoked bot codes that have an agent row, by name.
func (s *Service) ListBotAgents(userID int64) ([]BotAgentRow, error) {
	rows, err := s.db.Query(`SELECT a.code_id, c.name, a.version, a.host_name, a.windows_json, a.first_seen_at, a.last_seen_at
		FROM bot_agents a JOIN extension_codes c ON c.id = a.code_id
		WHERE a.user_id = ? AND c.user_id = a.user_id AND c.kind = 'bot' AND c.revoked_at IS NULL
		ORDER BY c.name COLLATE NOCASE, a.code_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BotAgentRow{}
	for rows.Next() {
		var a BotAgentRow
		if err := rows.Scan(&a.CodeID, &a.Name, &a.Version, &a.HostName, &a.WindowsJSON, &a.FirstSeenAt, &a.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
