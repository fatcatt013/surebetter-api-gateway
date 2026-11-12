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
}

var (
	windowIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	windowDevices   = map[string]bool{"desktop": true, "phone": true}
	// logged_out and blocked are reserved for later agent versions, accepted and shown already.
	windowStates = map[string]bool{"idle": true, "busy": true, "down": true, "logged_out": true, "blocked": true}
)

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func validPercent(p *int) bool { return p == nil || (*p >= 0 && *p <= 100) }

// validStatus checks a status's sizes and enums: at most 64 books (bookmaker 1–40 chars), version
// ≤ 64 chars, host name ≤ 64 chars, cpu and mem 0–100, at most 32 windows with unique ids
// ([a-z0-9-]{1,40}), a bookmaker (1–40 chars), number ≥ 1, a known device and state, proxy ≤ 100
// chars and reason ≤ 200 chars.
func validStatus(in botStatusIn) bool {
	if len(in.Books) > 64 || len(in.Windows) > 32 || runeLen(in.Version) > 64 {
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
			!windowDevices[w.Device] || !windowStates[w.State] || runeLen(w.Reason) > 200 || (w.Proxy != nil && runeLen(*w.Proxy) > 100) {
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
}

func statusOf(in botStatusIn) botStatus {
	st := botStatus{books: in.Books, dryRun: in.DryRun == nil || *in.DryRun, version: in.Version} // never Live by omission
	if st.books == nil {
		st.books = []botBook{}
	}
	if in.Host != nil {
		st.hostName, st.cpu, st.mem = in.Host.Name, in.Host.CPU, in.Host.Mem
	}
	if in.Windows == nil {
		in.Windows = []botWindow{}
	}
	js, _ := json.Marshal(in.Windows) // plain structs: cannot fail
	st.windowsJSON = string(js)
	return st
}

// setStatus stores an agent's last status in memory and its version, host name and windows in the
// registry. The app is told ({"type":"bot"}) when books, dry run, version, host name or windows
// changed, not for cpu/mem alone. false: codeID has no open bot socket.
func (b *botHub) setStatus(userID, codeID int64, st botStatus) bool {
	b.mu.Lock()
	p := b.presenceLocked(userID, codeID)
	if p == nil {
		b.mu.Unlock()
		return false
	}
	changed := p.dryRun != st.dryRun || !slices.Equal(p.books, st.books) || p.version != st.version ||
		p.hostName != st.hostName || p.windowsJSON != st.windowsJSON
	p.books, p.dryRun, p.version, p.hostName, p.windowsJSON = st.books, st.dryRun, st.version, st.hostName, st.windowsJSON
	p.cpu, p.mem = st.cpu, st.mem
	b.mu.Unlock()
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
