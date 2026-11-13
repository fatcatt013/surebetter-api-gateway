package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// botWindow is one browser window of an agent, as its status reports it and the Fleet shows it.
type botWindow struct {
	ID        string  `json:"id"`
	Bookmaker string  `json:"bookmaker"`
	Number    int     `json:"number"`
	Device    string  `json:"device"`
	State     string  `json:"state"`
	Extension bool    `json:"extension"`
	LoggedIn  *bool   `json:"loggedIn"`
	Proxy     *string `json:"proxy"`
	Reason    string  `json:"reason"`
	Mode      string  `json:"mode,omitempty"` // bot, login or stopped (protocol 2); absent for older agents
}

// botHost is the agent's machine. cpu and mem (percent) are kept in memory only.
type botHost struct {
	Name string `json:"name"`
	CPU  *int   `json:"cpu"`
	Mem  *int   `json:"mem"`
}

// botStatusIn is the POST /api/me/bot/status body. Fields an older agent leaves out stay empty.
type botStatusIn struct {
	Books   []botBook   `json:"books"`
	DryRun  *bool       `json:"dryRun"`
	Version string      `json:"version"`
	Host    *botHost    `json:"host"`
	Windows []botWindow `json:"windows"`
	Boot    string      `json:"boot"`  // 16 lowercase hex, random per agent process; "" = not sent
	Proto   int         `json:"proto"` // agent protocol: 2 takes orders with window ids and commands; 0 = not sent (Phase 1)
}

// botProto is the agent protocol the gateway sends orders and window commands to (and above).
const botProto = 2

var (
	bootPattern     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	windowIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	windowDevices   = map[string]bool{"desktop": true, "phone": true}
	// logged_out and blocked are reserved for later agent versions, accepted and shown already.
	windowStates = map[string]bool{"idle": true, "busy": true, "down": true, "logged_out": true, "blocked": true}
	// "" = an older agent that does not report modes.
	windowModes = map[string]bool{"": true, "bot": true, "login": true, "stopped": true}
)

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func validPercent(p *int) bool { return p == nil || (*p >= 0 && *p <= 100) }

// validStatus checks a status's sizes and enums: at most 64 books (bookmaker 1–40 chars), version
// ≤ 64 chars, host name ≤ 64 chars, cpu and mem 0–100, at most 32 windows with unique ids
// ([a-z0-9-]{1,40}), a bookmaker (1–40 chars), number ≥ 1, a known device and state, proxy ≤ 100
// chars, reason ≤ 200 chars and mode empty, bot, login or stopped, boot empty or 16 lowercase hex,
// and proto 0–99.
func validStatus(in botStatusIn) bool {
	if len(in.Books) > 64 || len(in.Windows) > 32 || runeLen(in.Version) > 64 || (in.Boot != "" && !bootPattern.MatchString(in.Boot)) ||
		in.Proto < 0 || in.Proto > 99 {
		return false
	}
	for _, b := range in.Books {
		if b.Bookmaker == "" || runeLen(b.Bookmaker) > 40 {
			return false
		}
	}
	if h := in.Host; h != nil && (runeLen(h.Name) > 64 || !validPercent(h.CPU) || !validPercent(h.Mem)) {
		return false
	}
	seen := map[string]bool{}
	for _, w := range in.Windows {
		if !windowIDPattern.MatchString(w.ID) || seen[w.ID] || w.Bookmaker == "" || runeLen(w.Bookmaker) > 40 || w.Number < 1 ||
			!windowDevices[w.Device] || !windowStates[w.State] || runeLen(w.Reason) > 200 || (w.Proxy != nil && runeLen(*w.Proxy) > 100) ||
			!windowModes[w.Mode] {
			return false
		}
		seen[w.ID] = true
	}
	return true
}

// botStatus is a validated status as the hub keeps it.
type botStatus struct {
	books       []botBook
	dryRun      bool
	version     string
	hostName    string
	cpu, mem    *int
	windowsJSON string
	windows     []botWindow
	boot        string
	proto       int
}

func statusOf(in botStatusIn) botStatus {
	st := botStatus{books: in.Books, dryRun: in.DryRun == nil || *in.DryRun, version: in.Version, boot: in.Boot, proto: in.Proto} // never Live by omission
	if st.books == nil {
		st.books = []botBook{}
	}
	if in.Host != nil {
		st.hostName, st.cpu, st.mem = in.Host.Name, in.Host.CPU, in.Host.Mem
	}
	if in.Windows == nil {
		in.Windows = []botWindow{}
	}
	st.windows = in.Windows
	js, _ := json.Marshal(in.Windows) // plain structs: cannot fail
	st.windowsJSON = string(js)
	return st
}

// setStatus stores an agent's last status in memory and its version, host name and windows in the
// registry. The app is told ({"type":"bot"}) when books, dry run, version, host name, windows or
// the protocol changed, not for cpu/mem alone. A status with a boot id other than the one the agent had when
// it was sent a still running order (or with one when that was empty) means the agent process
// restarted and can never report that order: the order ends as outcome_unknown ("agent
// restarted"), relayed like a posted final event, and the agent is freed. A status without a boot
// id ends nothing. false: codeID has no open bot socket.
func (b *botHub) setStatus(userID, codeID int64, st botStatus) bool {
	b.mu.Lock()
	p := b.presenceLocked(userID, codeID)
	if p == nil {
		b.mu.Unlock()
		return false
	}
	freed := b.pruneLocked()
	changed := p.dryRun != st.dryRun || !slices.Equal(p.books, st.books) || p.version != st.version ||
		p.hostName != st.hostName || p.windowsJSON != st.windowsJSON || p.proto != st.proto
	p.books, p.dryRun, p.version, p.hostName, p.windowsJSON = st.books, st.dryRun, st.version, st.hostName, st.windowsJSON
	p.windows, p.proto = st.windows, st.proto
	p.cpu, p.mem, p.boot = st.cpu, st.mem, st.boot
	var ended []endedOrder
	if st.boot != "" {
		for id, o := range b.orders {
			if o.userID == userID && o.codeID == codeID && o.boot != st.boot {
				delete(b.orders, id)
				ended = append(ended, endedOrder{id: id, arbKey: o.arbKey})
				changed = b.freeLocked(userID, codeID, id) || changed
			}
		}
	}
	b.mu.Unlock()
	b.notifyAll(freed)
	for _, o := range ended {
		log.Warn().Int64("user", userID).Int64("code", codeID).Str("order_id", o.id).Msg("bot order ended: agent restarted")
		b.relayEvent(userID, o.arbKey, botEventIn{OrderID: o.id, Event: "outcome_unknown", Reason: "agent restarted"})
	}
	if b.acc != nil {
		err := b.acc.SaveBotAgentStatus(userID, codeID, accounts.BotAgentStatus{Version: st.version, HostName: st.hostName, WindowsJSON: st.windowsJSON})
		if err != nil {
			log.Error().Err(err).Int64("code", codeID).Msg("bot agent status save failed")
		}
	}
	if changed {
		b.changed(userID)
	}
	return true
}

func (b *botHub) handleStatus(w http.ResponseWriter, r *http.Request) {
	var in botStatusIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || !validStatus(in) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_status")
		return
	}
	p := principalOf(r)
	if !b.setStatus(p.UserID, p.CodeID, statusOf(in)) {
		accounts.WriteError(w, http.StatusConflict, "bot_offline")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentView is one agent in the GET /api/me/bot/agents body.
type agentView struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	Online   bool        `json:"online"`
	Version  string      `json:"version"`
	Host     botHost     `json:"host"`
	LastSeen int64       `json:"lastSeen"`
	DryRun   bool        `json:"dryRun"`
	Busy     bool        `json:"busy"`
	Update   bool        `json:"update"` // online with an agent older than protocol 2: never sent orders or commands
	Windows  []botWindow `json:"windows"`
}

// windowsOf decodes a stored windows array; anything unreadable is no windows.
func windowsOf(js string) []botWindow {
	var ws []botWindow
	if err := json.Unmarshal([]byte(js), &ws); err != nil || ws == nil {
		return []botWindow{}
	}
	return ws
}

// fleet lists userID's agents for the Fleet: every non-revoked bot code with a registry row,
// online first, then by name. An online agent shows its last status from memory; an offline one
// its stored version, host name and windows, each window down ("Agent offline"), cpu/mem null,
// dry run, not busy. update: online and its last status (or none yet) is older than protocol 2.
func (b *botHub) fleet(userID int64) ([]agentView, error) {
	rows, err := b.acc.ListBotAgents(userID) // by name
	if err != nil {
		return nil, err
	}
	out := make([]agentView, 0, len(rows))
	b.mu.Lock()
	freed := b.pruneLocked()
	for _, row := range rows {
		v := agentView{ID: row.CodeID, Name: row.Name, LastSeen: row.LastSeenAt}
		if p := b.presenceLocked(userID, row.CodeID); p != nil {
			v.Online, v.DryRun, v.Busy, v.Version, v.Update = true, p.dryRun, p.busy != "", p.version, p.proto < botProto
			v.Host = botHost{Name: p.hostName, CPU: p.cpu, Mem: p.mem}
			v.Windows = windowsOf(p.windowsJSON)
		} else {
			v.DryRun, v.Version, v.Host = true, row.Version, botHost{Name: row.HostName}
			v.Windows = windowsOf(row.WindowsJSON)
			for i := range v.Windows {
				v.Windows[i].State, v.Windows[i].Reason = "down", "Agent offline"
			}
		}
		out = append(out, v)
	}
	b.mu.Unlock()
	b.notifyAll(freed)
	slices.SortStableFunc(out, func(x, y agentView) int {
		switch {
		case x.Online == y.Online:
			return 0
		case x.Online:
			return -1
		default:
			return 1
		}
	})
	return out, nil
}

func (b *botHub) handleAgents(w http.ResponseWriter, r *http.Request) {
	list, err := b.fleet(principalOf(r).UserID)
	if err != nil {
		log.Error().Err(err).Msg("bot agents list failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	accounts.WriteJSON(w, http.StatusOK, map[string][]agentView{"agents": list})
}
