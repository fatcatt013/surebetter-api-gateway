package main

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// The bot bankroll's hub side (bankroll Phase 1 contract): balance reads stored from the agents' statuses, ledger rows
// for the legs that were really placed, the backfill at start and the /api/me/bot/bankroll* routes. The money logic is
// in bot_bankroll.go.

// balanceEveryMs: an unchanged balance is stored again once its last stored row is this old (by the reads' times).
const balanceEveryMs = 3600 * 1000

// balMark is an account's last stored balance read.
type balMark struct {
	amount float64
	at     int64
}

// loadBalanceMarks reads codeID's last stored balance per window into b.balMarks, once per code (call without b.mu).
// A failed read is logged and tried again on the next status.
func (b *botHub) loadBalanceMarks(userID, codeID int64) {
	if b.acc == nil {
		return
	}
	b.mu.Lock()
	loaded := b.balLoaded[codeID]
	b.mu.Unlock()
	if loaded {
		return
	}
	rows, err := b.acc.LatestBotBalances(userID, codeID)
	if err != nil {
		log.Error().Err(err).Int64("code", codeID).Msg("bot balances read failed")
		return
	}
	b.mu.Lock()
	if !b.balLoaded[codeID] {
		for _, r := range rows {
			k := acctKey{r.CodeID, r.WindowID}
			if _, ok := b.balMarks[k]; !ok {
				b.balMarks[k] = balMark{r.Amount, r.At}
			}
		}
		b.balLoaded[codeID] = true
	}
	b.mu.Unlock()
}

// cents is v at 2 decimals, for comparing amounts.
func cents(v float64) int64 { return int64(math.Round(v * 100)) }

// recordBalancesLocked stores the balance reads of a protocol-7 status: a window's read becomes a row (with the
// account's point 0 the first time) when the window has no stored row yet, when the amount differs from the last
// stored one at 2 decimals, or when the last stored row is at least an hour older than the read. A read not newer
// than the last stored one, a null balance and an agent below protocol 7 store nothing. b.mu must be held.
func (b *botHub) recordBalancesLocked(userID, codeID int64, proto int, windows []botWindow, fx *effects) {
	if proto < balanceProto || b.acc == nil {
		return
	}
	for _, w := range windows {
		if w.Balance == nil || w.Balance.Null {
			continue
		}
		k := acctKey{codeID, w.ID}
		amount, at := w.Balance.Amount, w.Balance.At
		if m, ok := b.balMarks[k]; ok && (at <= m.at || (cents(amount) == cents(m.amount) && at-m.at < balanceEveryMs)) {
			continue
		}
		b.balMarks[k] = balMark{amount, at}
		row := accounts.BotBalanceRow{UserID: userID, CodeID: codeID, WindowID: w.ID, Bookmaker: w.Bookmaker, Amount: amount, At: at}
		fx.write(func() error { return b.acc.InsertBotBalance(row) })
	}
}

// ledgerLeg is leg i (l) of order orderID as a ledger row: the leg's window, bookmaker, label, odds and stake (after a
// final event: what the agent reported placing), the order's source, match and market, placed at placedAt, open.
func ledgerLeg(orderID string, userID, codeID int64, source string, info orderInfo, i int, l storedLeg, placedAt int64) accounts.BotLedgerLeg {
	return accounts.BotLedgerLeg{OrderID: orderID, Leg: i, UserID: userID, CodeID: codeID, WindowID: l.WindowID, Bookmaker: l.Bookmaker,
		Source: rowSource(source), Match: info.Match, Market: info.Market, MarketType: info.MarketType, Line: info.Line, Label: l.Label,
		Odds: l.Odds, Stake: l.Stake, PlacedAt: placedAt, Voidable: voidable(info.MarketType, info.Line), Result: "open", UpdatedAt: placedAt}
}

// placedLedgerLegs are the ledger rows of the legs reported placed of an order.
func placedLedgerLegs(orderID string, userID, codeID int64, source string, info orderInfo, legs []storedLeg, placedAt int64) []accounts.BotLedgerLeg {
	var out []accounts.BotLedgerLeg
	for i, l := range legs {
		if l.Placed != nil && *l.Placed {
			out = append(out, ledgerLeg(orderID, userID, codeID, source, info, i, l, placedAt))
		}
	}
	return out
}

// ledgerOnEndLocked queues the ledger rows of a live order that ended with legs reported placed (placed_at = now, the
// final event's time), after its row's end. A dry run adds none. b.mu must be held.
func (b *botHub) ledgerOnEndLocked(o *botOrder, now int64, fx *effects) {
	if o.dryRun || b.acc == nil {
		return
	}
	legs := placedLedgerLegs(o.id, o.userID, o.codeID, o.sourceOrApp(), o.info, o.legs, now)
	if len(legs) > 0 {
		fx.write(func() error { return b.acc.InsertBotLedgerLegs(legs) })
	}
}

// orderInfoOf decodes a stored order's info and legs (unreadable → empty).
func orderInfoOf(r accounts.BotOrderRow) (orderInfo, []storedLeg) {
	var info orderInfo
	var legs []storedLeg
	json.Unmarshal([]byte(r.InfoJSON), &info)
	json.Unmarshal([]byte(r.LegsJSON), &legs)
	return info, legs
}

// orderPlacedAt is a stored order's time for legs that have no event time: its end, else its send.
func orderPlacedAt(r accounts.BotOrderRow) int64 {
	if r.EndedAt > 0 {
		return r.EndedAt
	}
	return r.SentAt
}

// backfillLedger gives every stored live order with legs reported placed its ledger rows (placed_at = the order's end,
// else its send). Rows already there are kept, so running it at every start adds each leg once. Call at start, after
// recoverOrders.
func (b *botHub) backfillLedger() {
	if b.acc == nil {
		return
	}
	rows, err := b.acc.BotOrdersWithPlacedLegs()
	if err != nil {
		log.Error().Err(err).Msg("bot ledger backfill read failed")
		return
	}
	var legs []accounts.BotLedgerLeg
	for _, r := range rows {
		info, stored := orderInfoOf(r)
		legs = append(legs, placedLedgerLegs(r.ID, r.UserID, r.CodeID, r.Source, info, stored, orderPlacedAt(r))...)
	}
	b.dbMu.Lock()
	err = b.acc.InsertBotLedgerLegs(legs)
	b.dbMu.Unlock()
	if err != nil {
		log.Error().Err(err).Msg("bot ledger backfill failed")
		return
	}
	log.Info().Int("orders", len(rows)).Msg("bot ledger backfilled")
}

// loadLedger reads userID's ledger orders: every order with a ledger leg and every live order that needs a check
// (checkableOf), oldest id first.
func (b *botHub) loadLedger(userID int64) ([]*ledgerOrder, error) {
	legs, err := b.acc.BotLedgerLegs(userID)
	if err != nil {
		return nil, err
	}
	rows, err := b.acc.BotLedgerOrders(userID)
	if err != nil {
		return nil, err
	}
	byOrder := map[string][]accounts.BotLedgerLeg{}
	for _, l := range legs {
		byOrder[l.OrderID] = append(byOrder[l.OrderID], l)
	}
	out := make([]*ledgerOrder, 0, len(rows))
	for _, r := range rows {
		info, stored := orderInfoOf(r)
		out = append(out, &ledgerOrder{row: r, info: info, stored: stored, legs: byOrder[r.ID], check: checkableOf(r.State, r.DryRun, r.FlowJSON)})
	}
	return out, nil
}

// agentQuery reads ?agent= (the agent's code id, as the Fleet calls it): 0 = every agent; false: not a whole number ≥ 1.
func agentQuery(r *http.Request) (int64, bool) {
	s := r.URL.Query().Get("agent")
	if s == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id >= 1
}

// botCodeNames is userID's bot codes' names by id, revoked ones included (a removed agent keeps its bankroll).
func (b *botHub) botCodeNames(userID int64) (map[int64]string, error) {
	codes, err := b.acc.ListExtensionCodes(userID)
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	for _, c := range codes {
		if c.Kind == accounts.KindBot {
			names[c.ID] = c.Name
		}
	}
	return names, nil
}

// agentName is the name of agent id: its bot code's, else "#<id>" (a code deleted for good).
func agentName(names map[int64]string, id int64) string {
	if n, ok := names[id]; ok {
		return n
	}
	return "#" + strconv.FormatInt(id, 10)
}

// knownAgent: agent is one of the user's bot codes (names) or has bankroll rows (a code deleted for good).
func knownAgent(agent int64, names map[int64]string, accs []accounts.BotAccountRow, orders []*ledgerOrder) bool {
	_, ok := names[agent]
	return ok || slices.ContainsFunc(accs, func(a accounts.BotAccountRow) bool { return a.CodeID == agent }) ||
		slices.ContainsFunc(orders, func(o *ledgerOrder) bool { return o.row.CodeID == agent })
}

// bankrollAgent is one agent in GET /api/me/bot/bankroll: one with an account (point 0) or an order on the bankroll.
type bankrollAgent struct {
	AgentID  int64  `json:"agentId"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	Accounts int    `json:"accounts"` // its accounts with point 0
}

// bankrollAgents lists the agents with an account or an order, by name (case-insensitive), then id.
func bankrollAgents(accs []accounts.BotAccountRow, orders []*ledgerOrder, names map[int64]string, online map[int64]bool) []bankrollAgent {
	n := map[int64]int{}
	for _, a := range accs {
		n[a.CodeID]++
	}
	for _, o := range orders {
		if _, ok := n[o.row.CodeID]; !ok && o.row.CodeID != 0 {
			n[o.row.CodeID] = 0
		}
	}
	out := make([]bankrollAgent, 0, len(n))
	for id, count := range n {
		out = append(out, bankrollAgent{AgentID: id, Name: agentName(names, id), Online: online[id], Accounts: count})
	}
	slices.SortFunc(out, func(x, y bankrollAgent) int {
		if c := strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name)); c != 0 {
			return c
		}
		return int(x.AgentID - y.AgentID)
	})
	return out
}

// ofAgent keeps the items of agent (0 = all) by their code id.
func ofAgent[T any](items []T, agent int64, code func(T) int64) []T {
	if agent == 0 {
		return items
	}
	out := make([]T, 0, len(items))
	for _, it := range items {
		if code(it) == agent {
			out = append(out, it)
		}
	}
	return out
}

// bankrollView is the GET /api/me/bot/bankroll body.
type bankrollView struct {
	Agents     []bankrollAgent                `json:"agents"`
	Accounts   []bankrollAccount              `json:"accounts"`
	Books      []bankrollBook                 `json:"books"`
	Total      bankrollTotal                  `json:"total"`
	Counts     bankrollCounts                 `json:"counts"`
	Series     []bankrollPoint                `json:"series"`
	BookSeries map[string][]bankrollBookPoint `json:"bookSeries"`
}

// handleBankroll is GET /api/me/bot/bankroll?range=7d|30d|all (default 7d)&agent=: the cards at now (each account's
// newest balance: the stored one, or the agent's last status when that read is newer), the order counts, the curve over
// the range (event points) and each book's balance curve; agents lists every agent with an account or an order. agent
// (a code id) limits everything else to that agent's accounts and orders; 404 unknown_agent: not one of the user's.
func (b *botHub) handleBankroll(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "7d"
	}
	if _, ok := bankrollRanges[rng]; !ok {
		accounts.WriteError(w, http.StatusBadRequest, "bad_query")
		return
	}
	agent, ok := agentQuery(r)
	if !ok {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	accs, err := b.acc.BotAccounts(userID)
	var latest, hist []accounts.BotBalanceRow
	var orders []*ledgerOrder
	var agents []accounts.BotAgentRow
	var names map[int64]string
	var st accounts.BotSettings
	if err == nil {
		latest, err = b.acc.LatestBotBalances(userID, 0)
	}
	if err == nil {
		hist, err = b.acc.BotBalances(userID)
	}
	if err == nil {
		orders, err = b.loadLedger(userID)
	}
	if err == nil {
		agents, err = b.acc.ListBotAgents(userID)
	}
	if err == nil {
		names, err = b.botCodeNames(userID)
	}
	if err == nil {
		st, err = b.acc.BotSettings(userID)
	}
	if err != nil {
		log.Error().Err(err).Msg("bot bankroll read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if agent != 0 && !knownAgent(agent, names, accs, orders) {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	present := map[acctKey]bool{}
	for _, a := range agents {
		for _, win := range windowsOf(a.WindowsJSON) {
			present[acctKey{a.CodeID, win.ID}] = true
		}
	}
	idx := map[acctKey]int{}
	for i, row := range latest {
		idx[acctKey{row.CodeID, row.WindowID}] = i
	}
	online := map[int64]bool{}
	b.mu.Lock()
	for codeID, p := range b.agents[userID] {
		online[codeID] = true
		if p.proto < balanceProto {
			continue
		}
		for _, win := range p.windows {
			if win.Balance == nil || win.Balance.Null {
				continue
			}
			k := acctKey{codeID, win.ID}
			row := accounts.BotBalanceRow{UserID: userID, CodeID: codeID, WindowID: win.ID, Bookmaker: win.Bookmaker, Amount: win.Balance.Amount,
				At: win.Balance.At}
			if i, ok := idx[k]; !ok {
				idx[k] = len(latest)
				latest = append(latest, row)
			} else if row.At > latest[i].At {
				latest[i] = row
			}
		}
	}
	now := b.now().UnixMilli()
	b.mu.Unlock()
	list := bankrollAgents(accs, orders, names, online)
	accs = ofAgent(accs, agent, func(a accounts.BotAccountRow) int64 { return a.CodeID })
	latest = ofAgent(latest, agent, func(r accounts.BotBalanceRow) int64 { return r.CodeID })
	hist = ofAgent(hist, agent, func(r accounts.BotBalanceRow) int64 { return r.CodeID })
	orders = ofAgent(orders, agent, func(o *ledgerOrder) int64 { return o.row.CodeID })
	var from int64
	if rng != "all" {
		from = now - bankrollRanges[rng]
	}
	cards := bankrollFigures(accs, latest, orders, present, now, maxLegOf(st))
	series, books := bankrollSeries(accs, hist, orders, seriesTimes(hist, orders, from, now))
	accounts.WriteJSON(w, http.StatusOK, bankrollView{Agents: list, Accounts: cards.Accounts, Books: cards.Books, Total: cards.Total,
		Counts: cards.Counts, Series: series, BookSeries: books})
}

// bankrollLegView is one leg of an order in GET /api/me/bot/bankroll/orders. A leg of an order that needs a check and
// has no ledger row yet has placedAt null and result "open" (the PUT creates its row).
type bankrollLegView struct {
	Leg          int      `json:"leg"`
	Bookmaker    string   `json:"bookmaker"`
	WindowID     string   `json:"windowId"`
	Label        string   `json:"label"`
	Odds         float64  `json:"odds"`
	Stake        float64  `json:"stake"`
	PlacedAt     *int64   `json:"placedAt"`
	Result       *string  `json:"result"` // never null
	Payout       *float64 `json:"payout"`
	SettledAt    *int64   `json:"settledAt"`
	ResultSource string   `json:"resultSource"`
	TicketID     *string  `json:"ticketId"`
}

// bankrollOrderView is one order in GET /api/me/bot/bankroll/orders.
type bankrollOrderView struct {
	OrderID       string            `json:"orderId"`
	AgentID       int64             `json:"agentId"`
	AgentName     string            `json:"agentName"`
	Day           string            `json:"day"`           // the Prague date of createdAt (YYYY-MM-DD)
	PendingReason *string           `json:"pendingReason"` // status pending: outcome_unknown, stuck, voidable, one_sided or overdue
	Source        string            `json:"source"`
	Match         string            `json:"match"`
	Market        string            `json:"market"`
	MarketType    string            `json:"marketType"`
	Line          *float64          `json:"line"`
	Sport         string            `json:"sport"`
	Live          bool              `json:"live"`
	CreatedAt     int64             `json:"createdAt"`
	State         string            `json:"state"`
	Voidable      bool              `json:"voidable"`
	Status        string            `json:"status"`
	Booked        *float64          `json:"booked"`
	Profit        *float64          `json:"profit"`
	Legs          []bankrollLegView `json:"legs"`
}

// viewOf is o as the orders list shows it at now (its agent's name agent): its ledger legs (not-placed ones too) and,
// for an order that needs a check, its other stored legs (no row yet).
func (o *ledgerOrder) viewOf(now int64, agent string) (bankrollOrderView, orderEval) {
	e := o.eval(now)
	v := bankrollOrderView{OrderID: o.row.ID, AgentID: o.row.CodeID, AgentName: agent, Day: pragueDate(o.row.CreatedAt), Source: rowSource(o.row.Source), Match: o.info.Match, Market: o.info.Market,
		MarketType: o.info.MarketType, Line: o.info.Line, Sport: o.info.Sport, Live: o.info.IsLive || o.info.Live, CreatedAt: o.row.CreatedAt,
		State: o.row.State, Voidable: e.voidable, Status: e.status, Booked: e.booked, Profit: e.profit, Legs: []bankrollLegView{}}
	if e.status == "pending" {
		v.PendingReason = &e.reason
	}
	rows := map[int]accounts.BotLedgerLeg{}
	for _, l := range o.legs {
		rows[l.Leg] = l
	}
	for i, sl := range o.stored {
		l, ok := rows[i]
		switch {
		case ok:
			lv := bankrollLegView{Leg: i, Bookmaker: l.Bookmaker, WindowID: l.WindowID, Label: l.Label, Odds: l.Odds, Stake: l.Stake,
				PlacedAt: &l.PlacedAt, Result: &l.Result, Payout: l.Payout, SettledAt: l.SettledAt, ResultSource: l.ResultSource}
			if l.TicketID != "" {
				lv.TicketID = &l.TicketID
			}
			v.Legs = append(v.Legs, lv)
			delete(rows, i)
		case o.check:
			open := "open"
			v.Legs = append(v.Legs, bankrollLegView{Leg: i, Bookmaker: sl.Bookmaker, WindowID: sl.WindowID, Label: sl.Label, Odds: sl.Odds,
				Stake: sl.Stake, Result: &open})
		}
	}
	for _, l := range o.legs { // a row whose stored leg is missing (never expected): listed anyway
		if _, left := rows[l.Leg]; left {
			v.Legs = append(v.Legs, bankrollLegView{Leg: l.Leg, Bookmaker: l.Bookmaker, WindowID: l.WindowID, Label: l.Label, Odds: l.Odds,
				Stake: l.Stake, PlacedAt: &l.PlacedAt, Result: &l.Result, Payout: l.Payout, SettledAt: l.SettledAt, ResultSource: l.ResultSource})
		}
	}
	if v.Match == "" && len(o.legs) > 0 {
		v.Match, v.Market = o.legs[0].Match, o.legs[0].Market
	}
	return v, e
}

// bankrollOrdersView is the GET /api/me/bot/bankroll/orders body.
type bankrollOrdersView struct {
	Orders []bankrollOrderView `json:"orders"`
	Next   *string             `json:"next"`
}

const (
	bankrollOrdersLimit    = 50
	bankrollOrdersMaxLimit = 200
)

var bankrollStatuses = map[string]bool{"": true, "open": true, "booked": true, "settled": true, "pending": true}

// handleBankrollOrders is GET /api/me/bot/bankroll/orders?source=app|auto&bookmaker=&status=&from=&to=&limit=&before=&agent=:
// the ledger orders (and the live orders that need a check), newest first (created_at, then id). bookmaker: a leg at
// that book; status: the order's status, except open = an order with a real leg still open; from/to: created_at in
// [from, to) (Unix ms); before: the cursor "<createdAt>.<orderId>" of the last order of the previous page; agent: the
// orders sent to that agent (404 unknown_agent: not one of the user's).
func (b *botHub) handleBankrollOrders(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	q := r.URL.Query()
	source, book, status := q.Get("source"), q.Get("bookmaker"), q.Get("status")
	ok := (source == "" || source == sourceApp || source == sourceAuto) && runeLen(book) <= 40 && bankrollStatuses[status]
	num := func(key string, def int64) int64 {
		s := q.Get(key)
		if s == "" {
			return def
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			ok = false
		}
		return v
	}
	from, to, limit := num("from", 0), num("to", math.MaxInt64), num("limit", bankrollOrdersLimit)
	var curAt int64 = math.MaxInt64
	curID := ""
	if before := q.Get("before"); before != "" {
		at, id, found := strings.Cut(before, ".")
		v, err := strconv.ParseInt(at, 10, 64)
		if !found || err != nil || id == "" {
			ok = false
		}
		curAt, curID = v, id
	}
	if !ok || limit < 1 || limit > bankrollOrdersMaxLimit {
		accounts.WriteError(w, http.StatusBadRequest, "bad_query")
		return
	}
	agent, ok := agentQuery(r)
	if !ok {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	orders, err := b.loadLedger(userID)
	var names map[int64]string
	var accs []accounts.BotAccountRow
	if err == nil {
		names, err = b.botCodeNames(userID)
	}
	if err == nil && agent != 0 {
		accs, err = b.acc.BotAccounts(userID)
	}
	if err != nil {
		log.Error().Err(err).Msg("bot bankroll orders read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if agent != 0 && !knownAgent(agent, names, accs, orders) {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	orders = ofAgent(orders, agent, func(o *ledgerOrder) int64 { return o.row.CodeID })
	now := b.now().UnixMilli()
	var views []bankrollOrderView
	for _, o := range orders {
		if len(o.legs) == 0 && !o.check {
			continue
		}
		v, e := o.viewOf(now, agentName(names, o.row.CodeID))
		switch {
		case source != "" && v.Source != source,
			book != "" && !slices.ContainsFunc(v.Legs, func(l bankrollLegView) bool { return l.Bookmaker == book }),
			status == "open" && !e.open,
			status != "" && status != "open" && e.status != status,
			v.CreatedAt < from || v.CreatedAt >= to,
			v.CreatedAt > curAt || (v.CreatedAt == curAt && v.OrderID >= curID):
			continue
		}
		views = append(views, v)
	}
	slices.SortFunc(views, func(x, y bankrollOrderView) int {
		if x.CreatedAt != y.CreatedAt {
			if x.CreatedAt > y.CreatedAt {
				return -1
			}
			return 1
		}
		return -strings.Compare(x.OrderID, y.OrderID)
	})
	out := bankrollOrdersView{Orders: []bankrollOrderView{}}
	if int64(len(views)) > limit {
		views = views[:limit]
		last := views[limit-1]
		next := strconv.FormatInt(last.CreatedAt, 10) + "." + last.OrderID
		out.Next = &next
	}
	out.Orders = append(out.Orders, views...)
	accounts.WriteJSON(w, http.StatusOK, out)
}

// resultIn is the PUT /api/me/bot/bankroll/orders/{orderId}/legs/{leg} body.
type resultIn struct {
	Result    string   `json:"result"`
	Payout    *float64 `json:"payout"`
	SettledAt *int64   `json:"settledAt"`
}

// settleAheadMs: a manual result may be dated at most this far after now.
const settleAheadMs = 5 * 60 * 1000

// handleBankrollResult is PUT /api/me/bot/bankroll/orders/{orderId}/legs/{leg}: the owner sets a leg's result (source
// manual). payout is required (≥ 0) for cashed_out and ignored otherwise (computed from the leg's stake and odds; none
// for not_placed); settledAt defaults to now and may be at most 5 min ahead; open resets the leg. not_placed (the leg
// never reached the bookmaker) only on a leg of an order that needs a check. A leg of the user's live order that needs
// a check and has no row yet gets one from its stored leg (placed at the order's end, else its send). 404 not_found:
// no such leg of the user's; 400 bad_result: a bad body, or not_placed on another order. The user's app sockets get
// {"type":"bot"}.
func (b *botHub) handleBankrollResult(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	orderID := r.PathValue("orderId")
	leg, err := strconv.Atoi(r.PathValue("leg"))
	if err != nil || leg < 0 || leg > 1 || runeLen(orderID) > 64 {
		accounts.WriteError(w, http.StatusNotFound, "not_found")
		return
	}
	var in resultIn
	now := b.now().UnixMilli()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil || !ledgerResults[in.Result] ||
		(in.Result == "cashed_out" && (in.Payout == nil || !(*in.Payout >= 0) || math.IsInf(*in.Payout, 0))) ||
		(in.SettledAt != nil && (*in.SettledAt <= 0 || *in.SettledAt > now+settleAheadMs)) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_result")
		return
	}
	b.dbMu.Lock() // the row's creation and its result in one step against the hub's own ledger writes
	found, err := b.setLedgerResult(userID, orderID, leg, in, now)
	b.dbMu.Unlock()
	switch {
	case errors.Is(err, errNotCheck):
		accounts.WriteError(w, http.StatusBadRequest, "bad_result")
		return
	case err != nil:
		log.Error().Err(err).Msg("bot ledger result failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	case !found:
		accounts.WriteError(w, http.StatusNotFound, "not_found")
		return
	}
	log.Info().Int64("user", userID).Str("order_id", orderID).Int("leg", leg).Str("result", in.Result).Msg("bot ledger result set")
	b.changed(userID)
	w.WriteHeader(http.StatusNoContent)
}

// errNotCheck: not_placed on a leg of an order that does not need a check.
var errNotCheck = errors.New("not_placed on an order that needs no check")

// setLedgerResult stores in on userID's ledger leg (orderID, leg), creating the row first for a live order that needs
// a check. false: no such leg; errNotCheck: not_placed on an order that needs no check. b.dbMu must be held.
func (b *botHub) setLedgerResult(userID int64, orderID string, leg int, in resultIn, now int64) (bool, error) {
	l, found, err := b.acc.BotLedgerLegOf(userID, orderID, leg)
	if err != nil {
		return false, err
	}
	if !found || in.Result == notPlaced {
		row, ok, err := b.acc.BotOrderByID(userID, orderID)
		if err != nil || !ok {
			return false, err
		}
		check := !row.DryRun && checkableOf(row.State, row.DryRun, row.FlowJSON)
		if !check {
			if !found {
				return false, nil
			}
			return true, errNotCheck
		}
		if !found {
			info, stored := orderInfoOf(row)
			if leg >= len(stored) {
				return false, nil
			}
			l = ledgerLeg(row.ID, userID, row.CodeID, row.Source, info, leg, stored[leg], orderPlacedAt(row))
			l.UpdatedAt = now
			if err := b.acc.InsertBotLedgerLegs([]accounts.BotLedgerLeg{l}); err != nil {
				return false, err
			}
		}
	}
	var payout *float64
	var settled *int64
	source := ""
	if in.Result != "open" {
		payout = legPayout(in.Result, l.Stake, l.Odds, in.Payout)
		at := now
		if in.SettledAt != nil {
			at = *in.SettledAt
		}
		settled, source = &at, "manual"
	}
	return b.acc.SetBotLedgerResult(userID, orderID, leg, in.Result, payout, settled, source, now)
}
