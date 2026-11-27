package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
	"github.com/surebetter/api-gateway/internal/arbfilter"
)

// The picker (contract §5): every arb hub transition (queued) and every second (the scheduler's tick) each arb in
// the hub is evaluated for every user whose auto mode is on. Stage 1 (the preset at the bot's books) only counts;
// stage 2 (hidden, pauses, the match and arb rules, sizing, limits, agents) logs a line per arb life and reason, and
// in Dry run / Live creates and sends the order in the same botHub.mu section, so two arbs evaluated one after the
// other always see each other's order.

// arbSource is the arb hub as the picker reads it (a fake in tests).
type arbSource interface {
	Get(key string) (ArbOpportunity, bool)
	Items() []ArbOpportunity
}

// autoQueueSize is the hub-transition queue's buffer; a full queue drops the transition (the tick sees the arb).
const autoQueueSize = 4096

// autoPicker holds the picker's queue and memory. src, queue and kick are set once; users, cfgs, days and streak
// are guarded by botHub.mu; hidden by hiddenMu; logs by logMu (after botHub.mu when both are held).
type autoPicker struct {
	src   arbSource
	queue chan string
	kick  chan struct{}
	// active is how many users have auto mode on (no transition is queued while there are none).
	active   atomic.Int32
	dropped  atomic.Int64
	lastWarn atomic.Int64 // Unix ms of the last "queue full" warn

	users  map[int64]bool // users whose mode runs (not off)
	cfgs   map[int64]autoCfgCache
	days   map[int64]*autoDay
	streak map[int64]int       // auto orders in a row that ended stopped after accepted (memory only)
	dayErr map[int64]time.Time // when a day rebuild last failed and was logged

	hiddenMu  sync.Mutex
	hidden    map[int64]*hiddenSet
	hiddenGen map[int64]int       // bumped by each write of the user's "hidden" key
	hiddenErr map[int64]time.Time // when a read error was last logged

	logMu sync.Mutex
	logs  map[int64]*autoUserLog
}

func newAutoPicker() *autoPicker {
	return &autoPicker{queue: make(chan string, autoQueueSize), kick: make(chan struct{}, 1), users: map[int64]bool{},
		cfgs: map[int64]autoCfgCache{}, days: map[int64]*autoDay{}, streak: map[int64]int{}, dayErr: map[int64]time.Time{},
		hidden: map[int64]*hiddenSet{}, hiddenGen: map[int64]int{}, hiddenErr: map[int64]time.Time{}, logs: map[int64]*autoUserLog{}}
}

// observe is the arb hub's listener: an applied arb's key is queued without blocking (removals need nothing: a
// queued key is looked up again when it is evaluated). A full queue drops it, counted, with one warn a minute.
func (b *botHub) autoObserve(key string, _ ArbOpportunity, removed, _ bool) {
	p := b.auto
	if removed || p.active.Load() == 0 {
		return
	}
	select {
	case p.queue <- key:
	default:
		n := p.dropped.Add(1)
		now := time.Now().UnixMilli()
		if last := p.lastWarn.Load(); now-last >= time.Minute.Milliseconds() && p.lastWarn.CompareAndSwap(last, now) {
			log.Warn().Msgf("bot auto: queue full, %d dropped", n)
			p.dropped.Add(-n)
		}
	}
}

// autoKick asks the picker for a pass over every arb (the scheduler's 1 s tick), without blocking.
func (b *botHub) autoKick() {
	if b.auto.active.Load() == 0 {
		return
	}
	select {
	case b.auto.kick <- struct{}{}:
	default:
	}
}

// startAuto hooks the picker to the arb hub, reads which users have auto mode on and runs the picker until ctx ends.
// Call it before the NATS bridge starts.
func (b *botHub) startAuto(ctx context.Context, src arbSource) {
	b.auto.src = src
	if b.acc != nil {
		ids, err := b.acc.BotAutoUserIDs()
		if err != nil {
			log.Error().Err(err).Msg("bot auto: reading the users with auto settings failed")
		}
		for _, id := range ids {
			if b.loadSettings(id) != nil {
				continue
			}
			b.mu.Lock()
			b.autoActivateLocked(id)
			b.mu.Unlock()
		}
	}
	log.Info().Str("phase", b.autoPhase).Strs("modes", autoModesOf(b.autoPhase)).Int32("users", b.auto.active.Load()).Msg("bot auto mode ready")
	go b.runAuto(ctx)
}

func (b *botHub) runAuto(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case key := <-b.auto.queue:
			if opp, ok := b.auto.src.Get(key); ok {
				b.autoEvaluate([]ArbOpportunity{opp})
			}
		case <-b.auto.kick:
			b.autoEvaluate(b.auto.src.Items())
		}
	}
}

// autoActivateLocked records whether userID's mode runs (after a settings change or at start). b.mu must be held.
func (b *botHub) autoActivateLocked(userID int64) {
	_, mode := b.autoModeLocked(userID)
	if mode == autoOff {
		delete(b.auto.users, userID)
	} else {
		b.auto.users[userID] = true
	}
	b.auto.active.Store(int32(len(b.auto.users)))
}

// autoEvaluate evaluates each arb for every user whose auto mode runs.
func (b *botHub) autoEvaluate(opps []ArbOpportunity) {
	b.mu.Lock()
	users := make([]int64, 0, len(b.auto.users))
	for id := range b.auto.users {
		users = append(users, id)
	}
	b.mu.Unlock()
	slices.Sort(users)
	for _, userID := range users {
		if b.loadSettings(userID) != nil {
			continue
		}
		hidden, ok := b.autoHidden(userID)
		if !ok {
			continue // the Hidden list cannot be read: nothing is evaluated (an arb the user hid is never sent)
		}
		for _, opp := range opps {
			b.autoEvalOne(userID, opp, hidden)
		}
	}
}

// ── hidden ────────────────────────────────────────────────────────────────────

// hiddenSet is the user's Hidden list (account state key "hidden"): arb keys and matches.
type hiddenSet struct {
	arbs   map[string]bool
	events map[string]bool
}

// hiddenOf reads the state value of "hidden"; unreadable = nothing hidden.
func hiddenOf(raw json.RawMessage) *hiddenSet {
	h := &hiddenSet{arbs: map[string]bool{}, events: map[string]bool{}}
	var entries []struct {
		Kind    string `json:"kind"`
		ArbKey  string `json:"arbKey"`
		EventID string `json:"eventId"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return h
	}
	for _, e := range entries {
		switch {
		case e.Kind == "arb" && e.ArbKey != "":
			h.arbs[e.ArbKey] = true
		case e.Kind == "match" && e.EventID != "":
			h.events[e.EventID] = true
		}
	}
	return h
}

// autoStateWritten is the accounts state hook: a write of "hidden" makes the picker read it again.
func (b *botHub) autoStateWritten(userID int64, key string) {
	if key != "hidden" {
		return
	}
	b.auto.hiddenMu.Lock()
	delete(b.auto.hidden, userID)
	b.auto.hiddenGen[userID]++
	b.auto.hiddenMu.Unlock()
}

// autoHidden is userID's Hidden list, read from the account state at the first use and after each write of it (call
// without b.mu). false: it cannot be read (logged at most once a minute); the caller evaluates nothing for the user.
func (b *botHub) autoHidden(userID int64) (*hiddenSet, bool) {
	p := b.auto
	p.hiddenMu.Lock()
	h, gen := p.hidden[userID], p.hiddenGen[userID]
	p.hiddenMu.Unlock()
	if h != nil {
		return h, true
	}
	if b.acc == nil {
		return hiddenOf(nil), true
	}
	state, err := b.acc.GetState(userID)
	if err != nil {
		p.hiddenMu.Lock()
		if now := time.Now(); now.Sub(p.hiddenErr[userID]) >= time.Minute {
			p.hiddenErr[userID] = now
			log.Error().Err(err).Int64("user", userID).Msg("bot auto: hidden read failed; nothing evaluated")
		}
		p.hiddenMu.Unlock()
		return nil, false
	}
	h = hiddenOf(state["hidden"])
	p.hiddenMu.Lock()
	if p.hiddenGen[userID] == gen { // not written again meanwhile
		p.hidden[userID] = h
	}
	p.hiddenMu.Unlock()
	return h, true
}

// ── one arb for one user ──────────────────────────────────────────────────────

// toFilterArb is the arb with every field the filter, the re-pick and an order read.
func toFilterArb(o ArbOpportunity) arbfilter.Arb {
	stakes := make([]arbfilter.Stake, len(o.Stakes))
	for i, s := range o.Stakes {
		stakes[i] = arbfilter.Stake{Bookmaker: s.Bookmaker, OutcomeID: s.OutcomeID, Label: s.Label, DecimalOdds: s.DecimalOdds, StakePct: s.StakePct,
			MatchURL: s.MatchURL, MarketName: s.MarketName, SourceID: s.SourceID, SourceMarketRef: s.SourceMarketRef, FairOdds: s.FairOdds,
			MatchState: s.MatchState, MatchStateText: s.MatchStateText, MatchStateSince: s.MatchStateSince}
		for _, q := range s.Alternatives {
			stakes[i].Alternatives = append(stakes[i].Alternatives, arbfilter.Quote{Bookmaker: q.Bookmaker, Label: q.Label, DecimalOdds: q.DecimalOdds,
				MatchURL: q.MatchURL, MarketName: q.MarketName, SourceID: q.SourceID, SourceMarketRef: q.SourceMarketRef, FairOdds: q.FairOdds,
				MatchState: q.MatchState, MatchStateText: q.MatchStateText, MatchStateSince: q.MatchStateSince})
		}
	}
	return arbfilter.Arb{EventID: o.EventID, Sport: o.Sport, MarketType: o.MarketType, Line: o.Line, ProfitPct: o.ProfitPct, Stakes: stakes,
		DetectedAt: o.DetectedAt, FirstDetectedAt: o.FirstDetectedAt, LastChangedAt: o.LastChangedAt, NameHome: o.NameHome, NameAway: o.NameAway,
		League: o.League, MarketName: o.MarketName, StartTime: o.StartTime,
		MatchState: o.MatchState, MatchStateText: o.MatchStateText, MatchStateSince: o.MatchStateSince}
}

// orderArbKey is the app's arbKey: "<event_id>:<market_type>:<line>", the line written as JavaScript writes a number.
func orderArbKey(o ArbOpportunity) string {
	return o.EventID + ":" + o.MarketType + ":" + jsNumber(o.Line)
}

// jsNumber is a finite number as JavaScript's String(n) writes it in the range lines take ("2.5", "2", "-1.5", "0").
func jsNumber(v float64) string {
	if v == 0 {
		return "0" // -0 too
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// autoStage1 is the preset at the bot's books (contract §5.2): the arb re-picked among the filter's bookmakers ∩
// the auto Bookmakers limit (empty = all) ∩ botBooks, or the code of the first step it fails.
func autoStage1(a arbfilter.Arb, cfg autoSettings, botBooks []string, nowMs int64) (arbfilter.Arb, string) {
	if len(a.Stakes) != 2 {
		return a, "three_legs"
	}
	var books []string
	for _, id := range cfg.Filter.Bookmakers {
		if (len(cfg.Limits.Books) == 0 || slices.Contains(cfg.Limits.Books, id)) && slices.Contains(botBooks, id) && !slices.Contains(books, id) {
			books = append(books, id)
		}
	}
	if books == nil {
		books = []string{}
	}
	r, ok := arbfilter.ForBookList(a, books)
	if !ok || r.Stakes[0].Bookmaker == r.Stakes[1].Bookmaker { // an order needs two bookmakers
		return a, "books"
	}
	if code := arbfilter.Reject(r, *cfg.Filter, nowMs); code != "" {
		return r, code
	}
	return r, ""
}

// autoMinProfit is an auto order's minimum profit %: the filter's profit_min when above 0 (at most 100), else 2.
func autoMinProfit(f *arbfilter.Filter) float64 {
	if f != nil && f.ProfitMin != nil && *f.ProfitMin > 0 {
		return math.Min(100, *f.ProfitMin)
	}
	return 2
}

// autoEvalOne evaluates opp for userID: stage 1 (counted), then stage 2 (a log line, and in Dry run / Live the
// order), all in one b.mu section.
func (b *botHub) autoEvalOne(userID int64, opp ArbOpportunity, hidden *hiddenSet) {
	b.mu.Lock()
	fx := &effects{}
	defer b.release(fx)
	cfg, mode := b.autoModeLocked(userID)
	if mode == autoOff || cfg.Filter == nil || cfg.Bankroll == nil {
		return
	}
	if b.auto.src != nil { // the arb as the hub holds it now: a removed one is never evaluated, a changed one at its new prices
		cur, ok := b.auto.src.Get(oppStateKey(opp))
		if !ok {
			return
		}
		opp = cur
	}
	now := b.now()
	day, err := b.autoDayLocked(userID, fx)
	if err != nil {
		if t := b.auto.dayErr[userID]; now.Sub(t) >= time.Minute {
			b.auto.dayErr[userID] = now
			log.Error().Err(err).Int64("user", userID).Msg("bot auto: reading today's orders failed; nothing evaluated")
		}
		return
	}
	r, code := autoStage1(toFilterArb(opp), cfg, b.botBooksLocked(userID), now.UnixMilli())
	if code != "" {
		b.auto.count(userID, b.autoDayFrom(), code)
		return
	}
	line := b.autoStage2Locked(userID, opp, r, cfg, mode, day, hidden, now, fx)
	line.life = oppStateKey(opp) + "@" + strconv.FormatInt(r.FirstSeen(), 10)
	b.auto.record(userID, b.autoDayFrom(), line, now.UnixMilli())
}

// fmtKc writes whole crowns with a space every three digits ("2 000").
func fmtKc(v int64) string {
	s := strconv.FormatInt(v, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// fmtPct writes a percent with at most 2 decimals ("1.6", "2").
func fmtPct(v float64) string { return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64) }

// secondsLeft is d in whole seconds, rounded up.
func secondsLeft(d time.Duration) int64 { return int64(math.Ceil(d.Seconds())) }

// truncRunes cuts s to at most n characters.
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// autoOrderIn is the order the app's row would send for the re-picked arb r with stakes (botOrderLegs /
// botOrderInfo), with the auto bankroll and minimum profit; the order's match state is r's, each leg's its stake's
// (a state the order body would refuse goes as "", like the app's absent one).
func autoOrderIn(opp ArbOpportunity, r arbfilter.Arb, stakes []float64, cfg autoSettings) botOrderIn {
	live := arbfilter.IsLive(opp.Sport)
	line := opp.Line
	rounding := cfg.Bankroll.Rounding
	br := *cfg.Bankroll
	minProfit := autoMinProfit(cfg.Filter)
	in := botOrderIn{ArbKey: orderArbKey(opp), Match: truncRunes(opp.NameHome+" – "+opp.NameAway, 200), Market: truncRunes(opp.MarketName, 200),
		Live: live, ProfitPct: r.ProfitPct, MarketType: truncRunes(opp.MarketType, 64), Line: &line, IsLive: live,
		Sport: truncRunes(arbfilter.BaseSport(opp.Sport), 40), Rounding: &rounding, MinProfitPct: &minProfit, Bankroll: &br,
		EventID: truncRunes(opp.EventID, 128), MatchState: orderMatchState(r.MatchState)}
	for i, s := range r.Stakes {
		market := s.MarketName
		if market == "" {
			market = opp.MarketName
		}
		label := s.Label
		if label == "" {
			label = s.OutcomeID
		}
		var fair *float64
		if s.FairOdds != nil && *s.FairOdds > 1 {
			v := *s.FairOdds
			fair = &v
		}
		in.Legs = append(in.Legs, botLeg{Bookmaker: s.Bookmaker, MatchURL: s.MatchURL, SourceID: s.SourceID, SourceMarketRef: s.SourceMarketRef,
			MarketName: market, Label: label, Odds: s.DecimalOdds, Stake: stakes[i], OutcomeID: truncRunes(s.OutcomeID, 120), FairOdds: fair,
			MatchState: orderMatchState(s.MatchState), MatchStateText: truncRunes(s.MatchStateText, maxMatchStateText), MatchStateSince: max(s.MatchStateSince, 0)})
	}
	return in
}

// autoStage2Locked runs the logged checks of contract §5.3 in order on the re-picked arb r and returns the line:
// the first failing check's skip line, else would_send (Log only) or, in Dry run / Live, sent with the order (made
// and sent here) or no_free_window when no agent took it. b.mu must be held.
func (b *botHub) autoStage2Locked(userID int64, opp ArbOpportunity, r arbfilter.Arb, cfg autoSettings, mode string, day *autoDay,
	hidden *hiddenSet, now time.Time, fx *effects) autoLogLine {
	if len(b.ordersLocked(userID, orderWaiting)) > 0 {
		b.dispatchLocked(userID, fx) // the app's waiting orders take a free window first: auto never delays them
	}
	st := b.settingsLocked(userID)
	dry := autoDryOf(mode, st)
	key := orderArbKey(opp)
	nowMs := now.UnixMilli()
	line := autoLogLine{Kind: "skip", ArbKey: key, EventID: opp.EventID, Match: opp.NameHome + " – " + opp.NameAway, Market: opp.MarketName,
		Sport: arbfilter.BaseSport(opp.Sport), Live: arbfilter.IsLive(opp.Sport), ProfitPct: r.ProfitPct, Legs: []autoLogLeg{}}
	for _, s := range r.Stakes {
		line.Legs = append(line.Legs, autoLogLeg{Bookmaker: s.Bookmaker, Odds: s.DecimalOdds})
	}
	skip := func(code, text string) autoLogLine {
		line.Reason, line.Text = code, text
		return line
	}
	ev := truncRunes(opp.EventID, 128) // as the order stores it
	onMatch := func(rec *autoRec) bool { return ev != "" && rec.eventID == ev }

	if hidden.arbs[key] || hidden.events[ev] {
		return skip("hidden", "Hidden")
	}
	for _, s := range r.Stakes {
		if s.SourceID == "" || s.MatchURL == "" {
			return skip("no_selection_id", "No selection id at "+bookLabel(s.Bookmaker))
		}
	}
	if st.Paused {
		return skip("bots_paused", "Bots paused")
	}
	if st.AutoPausedReason != "" {
		return skip("auto_paused", "Auto paused")
	}
	for _, rec := range day.recs {
		if onMatch(rec) && rec.state == "placed" && (!rec.dry || (dry && rec.source == sourceAuto)) {
			return skip("match_placed", "Already placed on this match today")
		}
	}
	if rec := day.lastRec(func(rec *autoRec) bool {
		return onMatch(rec) && !rec.dry && (rec.state == "outcome_unknown" || rec.oneSided() || rec.stuck)
	}); rec != nil {
		return skip("match_blocked", "Match blocked today · order "+shortID(rec.id))
	}
	running, autoRunning := 0, 0
	for _, o := range b.orders {
		if o.userID != userID {
			continue
		}
		if o.arbKey == key {
			return skip("order_open", "Order open on this arb")
		}
		if o.state == orderRunning {
			running++
			if o.isAuto() {
				autoRunning++
			}
		}
	}
	for _, o := range b.orders {
		if o.userID == userID && o.state == orderRunning && ev != "" && o.eventID == ev {
			return skip("match_running", "Order running on this match")
		}
	}
	var cool time.Duration // the longest cool-down left of an auto order on the match that stopped after it was accepted
	for _, recs := range []map[string]*autoRec{day.recs, day.prev} {
		for _, rec := range recs {
			if onMatch(rec) && rec.source == sourceAuto && rec.state == "stopped" && rec.accepted {
				cool = max(cool, time.UnixMilli(rec.ended).Add(autoCooldown).Sub(now))
			}
		}
	}
	if cool > 0 {
		return skip("match_cooldown", fmt.Sprintf("Match cool-down · %d s left", secondsLeft(cool)))
	}
	if rec := day.lastRec(func(rec *autoRec) bool { return rec.arbKey == key && rec.source == sourceAuto }); rec != nil && rec.state == "stopped" {
		changed := opp.LastChangedAt
		if changed == 0 {
			changed = r.FirstSeen()
		}
		if changed <= rec.ended && rec.firstSeen == r.FirstSeen() {
			return skip("same_prices", "Same prices as order "+shortID(rec.id))
		}
	}
	tries := 0
	for _, rec := range day.recs {
		if rec.arbKey == key && rec.source == sourceAuto && rec.dry == dry {
			tries++
		}
	}
	if tries >= autoTries {
		return skip("arb_attempts", fmt.Sprintf("%d tries on this arb today", tries))
	}

	// Sizing: the app's computeStakes with the auto bankroll; the rounded stakes must keep the minimum profit.
	br := arbfilter.Bankroll{Mode: cfg.Bankroll.Mode, Total: cfg.Bankroll.Total, MaxLeg: cfg.Bankroll.MaxLeg, AnchorBook: cfg.Bankroll.AnchorBook,
		Rounding: cfg.Bankroll.Rounding}
	plan := arbfilter.ComputeStakes(r, br)
	worst, total := math.Inf(1), 0.0
	for i, s := range r.Stakes {
		line.Legs[i].Stake = plan.Stakes[i]
		worst = math.Min(worst, plan.Stakes[i]*s.DecimalOdds)
		total += plan.Stakes[i]
	}
	roi := 0.0
	if total > 0 {
		roi = (worst - total) / total * 100
	}
	if minProfit := autoMinProfit(cfg.Filter); roi < minProfit {
		return skip("rounded_below_min", "Rounded "+fmtPct(roi)+" % < "+fmtPct(minProfit)+" %")
	}
	stakes := plan.Stakes
	for i, s := range r.Stakes {
		if pb := cfg.Limits.PerBook[s.Bookmaker]; pb.MinStake > 0 && stakes[i] < float64(pb.MinStake) {
			return skip("stake_under_min", "Under "+bookLabel(s.Bookmaker)+"'s minimum stake ("+fmtKc(pb.MinStake)+" Kč)")
		}
	}
	for i, s := range r.Stakes {
		if pb := cfg.Limits.PerBook[s.Bookmaker]; pb.MaxStake > 0 && stakes[i] > float64(pb.MaxStake) {
			return skip("stake_over_max", "Over "+bookLabel(s.Bookmaker)+"'s maximum stake ("+fmtKc(pb.MaxStake)+" Kč)")
		}
	}
	if st.MaxLeg > 0 && (stakes[0] > float64(st.MaxLeg) || stakes[1] > float64(st.MaxLeg)) {
		return skip("limit_leg", "Over the leg limit")
	}
	if st.MaxOrder > 0 && total > float64(st.MaxOrder) {
		return skip("limit_order", "Over the order limit")
	}
	autoCount, placedCount, autoStake, liveStake := 0, 0, int64(0), int64(0)
	for _, rec := range day.recs {
		if rec.source == sourceAuto && rec.dry == dry {
			autoCount++
			if rec.state == "placed" { // a dry run's successful end is placed too; one-sided ends are not
				placedCount++
			}
		}
		if !rec.dry {
			liveStake += rec.stake
			if rec.source == sourceAuto {
				autoStake += rec.stake
			}
		}
	}
	if cfg.Limits.PerDay > 0 && autoCount >= cfg.Limits.PerDay {
		return skip("auto_day_count", fmt.Sprintf("Auto orders today: %d", autoCount))
	}
	if cfg.Limits.PlacedPerDay > 0 && placedCount >= cfg.Limits.PlacedPerDay {
		return skip("auto_day_placed", fmt.Sprintf("Placed arbs today: %d", placedCount))
	}
	stake := int64(math.Round(total))
	if !dry && cfg.Limits.StakePerDay > 0 && autoStake+stake > cfg.Limits.StakePerDay {
		return skip("auto_day_stake", "Over the auto stake per day")
	}
	if !dry {
		for i, s := range r.Stakes {
			pb := cfg.Limits.PerBook[s.Bookmaker]
			if pb.StakePerDay <= 0 {
				continue
			}
			used := 0.0
			for _, rec := range day.recs {
				if rec.dry {
					continue
				}
				for _, l := range rec.legs {
					if l.Bookmaker == s.Bookmaker && rec.legCounts(l) {
						used += l.Stake
					}
				}
			}
			if used+stakes[i] > float64(pb.StakePerDay) {
				return skip("book_day_stake", "Over "+bookLabel(s.Bookmaker)+"'s stake per day")
			}
		}
	}
	if !dry && st.PerDay > 0 && liveStake+stake > st.PerDay {
		return skip("limit_day", "Over the day limit")
	}
	if autoRunning >= cfg.Limits.AtOnce {
		return skip("auto_at_once", fmt.Sprintf("Auto orders at once: %d running", autoRunning))
	}
	if running >= st.OrdersAtOnce {
		return skip("orders_at_once", fmt.Sprintf("Orders at once: %d running", running))
	}
	var lastSent int64 // the last auto send, today or in the hour before (the gap is at most an hour)
	for _, recs := range []map[string]*autoRec{day.recs, day.prev} {
		for _, rec := range recs {
			if rec.source == sourceAuto {
				lastSent = max(lastSent, rec.sent)
			}
		}
	}
	if lastSent > 0 {
		if left := time.UnixMilli(lastSent).Add(time.Duration(cfg.Limits.GapSec) * time.Second).Sub(now); left > 0 {
			return skip("gap", fmt.Sprintf("Gap · %d s left", secondsLeft(left)))
		}
	}
	online := false
	for _, p := range b.agents[userID] {
		online = online || p.proto >= botProto
	}
	if !online {
		return skip("no_agent", "No agent online")
	}
	in := autoOrderIn(opp, r, stakes, cfg)
	if !validOrder(in) { // the checks above leave only absurd bankrolls (a stake over 10 000 000 Kč)
		log.Warn().Int64("user", userID).Str("arb_key", key).Msg("bot auto: the order would be invalid; skipped")
		return skip("limit_leg", "Over the leg limit")
	}
	o := &botOrder{id: newOrderID(), userID: userID, arbKey: key, state: orderWaiting, dryRun: dry, stake: stake, info: infoOf(in),
		created: now, source: sourceAuto, eventID: in.EventID, forceDry: mode == autoDry}
	o.info.FirstDetectedAt = r.FirstSeen()
	o.oddsDrop = oddsDropOf(st, o.info)
	for _, l := range in.Legs {
		o.legs = append(o.legs, storedLeg{botLeg: l})
	}
	noWindow := func() autoLogLine {
		missing, short := b.blockedLocked(o)
		if short != nil {
			return skip(short.skipReason())
		}
		if len(missing) == 1 {
			return skip("no_free_window", "No free "+bookLabel(missing[0])+" window")
		}
		return skip("no_free_window", "No free "+bookLabel(missing[0])+" and "+bookLabel(missing[1])+" window")
	}
	if mode == autoLog {
		if _, p, _ := b.pickLocked(o, b.reservedLocked(userID), nil); p == nil {
			return noWindow()
		}
		line.Kind = "would_send"
		line.Text = "Would send"
		return line
	}
	if !b.sendAutoLocked(o, st, nowMs, fx) {
		return noWindow()
	}
	line.Kind, line.Text, line.OrderID = "sent", "Sent · order "+shortID(o.id), o.id
	return line
}

// sendAutoLocked sends the new auto order o now or not at all: an agent with a free window of both bookmakers whose
// socket takes it. Sent: o is a running order with its row (source auto, its match) and the app is told. Not sent: no
// row, nothing kept. b.mu must be held.
func (b *botHub) sendAutoLocked(o *botOrder, st accounts.BotSettings, nowMs int64, fx *effects) bool {
	reserved := b.reservedLocked(o.userID)
	skip := map[int64]bool{}
	for {
		codeID, p, wins := b.pickLocked(o, reserved, skip)
		if p == nil {
			return false
		}
		if !b.trySendLocked(o, codeID, p, wins, st) {
			skip[codeID] = true
			continue
		}
		b.orders[o.id] = o
		if b.acc != nil {
			row := accounts.BotOrderRow{ID: o.id, UserID: o.userID, ArbKey: o.arbKey, CodeID: codeID, State: orderRunning, DryRun: o.dryRun,
				Stake: o.stake, InfoJSON: mustJSON(o.info), LegsJSON: o.legsJSON(), CreatedAt: nowMs, SentAt: o.sent.UnixMilli(),
				OddsDrop: o.oddsDrop, MinProfitPct: o.info.MinProfitPct, Source: sourceAuto, EventID: o.eventID}
			fx.write(func() error { return b.acc.InsertBotOrder(row) })
		}
		b.sentLocked(o, fx)
		return true
	}
}
