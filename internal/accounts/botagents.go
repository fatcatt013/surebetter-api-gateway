package accounts

import (
	"database/sql"
	"errors"
)

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
	HasFlows    bool  // the agent has uploaded its Flows
	FlowsAt     int64 // when its Flows last changed; 0 = never
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
	rows, err := s.db.Query(`SELECT a.code_id, c.name, a.version, a.host_name, a.windows_json, a.first_seen_at, a.last_seen_at,
			a.flows_json <> '', a.flows_at
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
		if err := rows.Scan(&a.CodeID, &a.Name, &a.Version, &a.HostName, &a.WindowsJSON, &a.FirstSeenAt, &a.LastSeenAt, &a.HasFlows, &a.FlowsAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveBotAgentFlows stores codeID's Flows upload (inserting the agent's row if it has none yet).
// flows_at moves only when the JSON differs from the stored one; changed reports that.
func (s *Service) SaveBotAgentFlows(userID, codeID int64, flowsJSON string) (changed bool, err error) {
	now := s.nowMs()
	res, err := s.db.Exec(`INSERT INTO bot_agents (code_id, user_id, first_seen_at, last_seen_at, flows_json, flows_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (code_id) DO UPDATE SET flows_json = excluded.flows_json, flows_at = excluded.flows_at
			WHERE bot_agents.flows_json <> excluded.flows_json`, codeID, userID, now, now, flowsJSON, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// BotAgentFlows returns codeID's stored Flows and when they last changed; "" and 0 when it has
// none or is not userID's agent.
func (s *Service) BotAgentFlows(userID, codeID int64) (string, int64, error) {
	var flows string
	var at int64
	err := s.db.QueryRow(`SELECT flows_json, flows_at FROM bot_agents WHERE code_id = ? AND user_id = ?`, codeID, userID).Scan(&flows, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return flows, at, err
}

// BotAgentLimitIndex returns codeID's limit index (a JSON array, "[]" when none was saved); found
// false when codeID has no agent row of userID.
func (s *Service) BotAgentLimitIndex(userID, codeID int64) (js string, found bool, err error) {
	err = s.db.QueryRow(`SELECT limit_index_json FROM bot_agents WHERE code_id = ? AND user_id = ?`, codeID, userID).Scan(&js)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return js, err == nil, err
}

// SaveBotAgentLimitIndex replaces codeID's limit index with js (a validated JSON array); found false
// when codeID has no agent row of userID (nothing is written).
func (s *Service) SaveBotAgentLimitIndex(userID, codeID int64, js string) (found bool, err error) {
	res, err := s.db.Exec(`UPDATE bot_agents SET limit_index_json = ? WHERE code_id = ? AND user_id = ?`, js, codeID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
