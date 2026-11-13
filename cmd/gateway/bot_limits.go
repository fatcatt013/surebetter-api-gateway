package main

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// limitEntry is one entry of an agent's limit index (rule 3): the level of bookmaker for sport and
// the market types it lists. Level 0 Normal, 1 Some limits, 2 Strict.
type limitEntry struct {
	Bookmaker   string   `json:"bookmaker"`
	Sport       string   `json:"sport"`
	MarketTypes []string `json:"marketTypes"`
	Level       int      `json:"level"`
}

// Limits of a limit index.
const (
	maxLimitEntries     = 1000
	maxLimitMarketTypes = 50
	maxLimitIndexBody   = 4 << 20
)

// limitIndexOf checks entries and returns them in the stored shape (marketTypes never null); false:
// over 1000 entries, a bookmaker not [a-z0-9]{1,40}, a sport over 40 characters, over 50 market
// types or one over 64 characters, or a level other than 0, 1 or 2.
func limitIndexOf(entries []limitEntry) ([]limitEntry, bool) {
	if len(entries) > maxLimitEntries {
		return nil, false
	}
	out := make([]limitEntry, 0, len(entries))
	for _, e := range entries {
		if !bookmakerPattern.MatchString(e.Bookmaker) || runeLen(e.Sport) > 40 || len(e.MarketTypes) > maxLimitMarketTypes ||
			e.Level < 0 || e.Level > 2 {
			return nil, false
		}
		for _, mt := range e.MarketTypes {
			if runeLen(mt) > 64 {
				return nil, false
			}
		}
		if e.MarketTypes == nil {
			e.MarketTypes = []string{}
		}
		out = append(out, e)
	}
	return out, true
}

// limitIndexFrom decodes a stored limit index; anything unreadable is an empty index.
func limitIndexFrom(js string) []limitEntry {
	var entries []limitEntry
	if json.Unmarshal([]byte(js), &entries) != nil {
		return []limitEntry{}
	}
	out, ok := limitIndexOf(entries)
	if !ok {
		return []limitEntry{}
	}
	return out
}

// limitLevel is book's level in idx for sport and marketType: the first entry with that bookmaker,
// that sport and marketType in its market types; none → 0 (Normal).
func limitLevel(idx []limitEntry, book, sport, marketType string) int {
	for _, e := range idx {
		if e.Bookmaker == book && e.Sport == sport && slices.Contains(e.MarketTypes, marketType) {
			return e.Level
		}
	}
	return 0
}

// limitLevelsLocked is each leg's level of o in agent codeID's cached limit index (0 when it is not
// cached). b.mu must be held.
func (b *botHub) limitLevelsLocked(codeID int64, o *botOrder) [2]int {
	var out [2]int
	for i := range out {
		if i < len(o.legs) {
			out[i] = limitLevel(b.limits[codeID], o.legs[i].Bookmaker, o.info.Sport, o.info.MarketType)
		}
	}
	return out
}

// loadLimitIndex caches agent codeID's limit index if it is not cached yet (call without b.mu). A
// read error or a missing agent row leaves it uncached (its legs go out at level 0 until a later
// load succeeds). A PUT replaces the cached index, so a load never overwrites one.
func (b *botHub) loadLimitIndex(userID, codeID int64) {
	if b.acc == nil {
		return
	}
	b.mu.Lock()
	_, ok := b.limits[codeID]
	b.mu.Unlock()
	if ok {
		return
	}
	js, found, err := b.acc.BotAgentLimitIndex(userID, codeID)
	if err != nil {
		log.Error().Err(err).Int64("code", codeID).Msg("bot limit index read failed")
		return
	}
	if !found {
		return
	}
	idx := limitIndexFrom(js)
	b.mu.Lock()
	if _, ok := b.limits[codeID]; !ok {
		b.limits[codeID] = idx
	}
	b.mu.Unlock()
}

// limitIndexView is the GET and PUT /api/me/bot/agents/{agentId}/limit-index body.
type limitIndexView struct {
	Entries []limitEntry `json:"entries"`
}

// limitAgent reads {agentId} and checks it is one of the caller's (non-revoked) bot codes; false:
// the answer (404 unknown_agent or 500) was written.
func (b *botHub) limitAgent(w http.ResponseWriter, r *http.Request) (int64, bool) {
	codeID, ok := agentParam(r)
	if !ok {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return 0, false
	}
	if ok, err := b.agentOf(principalOf(r).UserID, codeID); err != nil {
		log.Error().Err(err).Msg("bot code lookup failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return 0, false
	} else if !ok {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return 0, false
	}
	return codeID, true
}

// handleGetLimitIndex is GET /api/me/bot/agents/{agentId}/limit-index: {"entries":[…]}. 404
// unknown_agent: not one of the user's agents (a bot code that never connected has none).
func (b *botHub) handleGetLimitIndex(w http.ResponseWriter, r *http.Request) {
	codeID, ok := b.limitAgent(w, r)
	if !ok {
		return
	}
	js, found, err := b.acc.BotAgentLimitIndex(principalOf(r).UserID, codeID)
	if err != nil {
		log.Error().Err(err).Msg("bot limit index read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !found {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	accounts.WriteJSON(w, http.StatusOK, limitIndexView{Entries: limitIndexFrom(js)})
}

// handlePutLimitIndex is PUT /api/me/bot/agents/{agentId}/limit-index {"entries":[…]}: it replaces
// the agent's index (stored, and cached for the orders sent from now on) and answers it as stored.
// 400 bad_limit_index: not that shape or an entry out of range (limitIndexOf); 404 unknown_agent.
func (b *botHub) handlePutLimitIndex(w http.ResponseWriter, r *http.Request) {
	codeID, ok := agentParam(r)
	if !ok {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	var in struct {
		Entries *[]limitEntry `json:"entries"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLimitIndexBody)).Decode(&in); err != nil || in.Entries == nil {
		accounts.WriteError(w, http.StatusBadRequest, "bad_limit_index")
		return
	}
	idx, valid := limitIndexOf(*in.Entries)
	if !valid {
		accounts.WriteError(w, http.StatusBadRequest, "bad_limit_index")
		return
	}
	if codeID, ok = b.limitAgent(w, r); !ok {
		return
	}
	// limitMu keeps the stored and the cached index the same when two PUTs race.
	b.limitMu.Lock()
	defer b.limitMu.Unlock()
	found, err := b.acc.SaveBotAgentLimitIndex(principalOf(r).UserID, codeID, mustJSON(idx))
	if err != nil {
		log.Error().Err(err).Msg("bot limit index save failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !found {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	b.mu.Lock()
	b.limits[codeID] = idx
	b.mu.Unlock()
	accounts.WriteJSON(w, http.StatusOK, limitIndexView{Entries: idx})
}
