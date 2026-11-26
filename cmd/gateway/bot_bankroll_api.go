package main

import (
	"encoding/json"
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

// bankrollView is the GET /api/me/bot/bankroll body.
type bankrollView struct {
	Accounts []bankrollAccount `json:"accounts"`
	Books    []bankrollBook    `json:"books"`
	Total    bankrollTotal     `json:"total"`
	Series   []bankrollPoint   `json:"series"`
}

// handleBankroll is GET /api/me/bot/bankroll?range=7d|30d|all (default 7d): the cards at now (each account's newest
// balance: the stored one, or the agent's last status when that read is newer) and the curve over the range.
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
	accs, err := b.acc.BotAccounts(userID)
	var latest, hist []accounts.BotBalanceRow
	var orders []*ledgerOrder
	var agents []accounts.BotAgentRow
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
	if err != nil {
		log.Error().Err(err).Msg("bot bankroll read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
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
	b.mu.Lock()
	for codeID, p := range b.agents[userID] {
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
	var earliest int64
	first := func(t int64) {
		if t > 0 && (earliest == 0 || t < earliest) {
			earliest = t
		}
	}
	for _, a := range accs {
		first(a.StartAt)
	}
	for _, o := range orders {
		for _, l := range o.legs {
			first(l.PlacedAt)
		}
	}
	cards := bankrollFigures(accs, latest, orders, present, now)
	accounts.WriteJSON(w, http.StatusOK, bankrollView{Accounts: cards.Accounts, Books: cards.Books, Total: cards.Total,
		Series: bankrollSeries(accs, hist, orders, seriesPoints(rng, now, earliest))})
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
	OrderID    string            `json:"orderId"`
	Source     string            `json:"source"`
	Match      string            `json:"match"`
	Market     string            `json:"market"`
	MarketType string            `json:"marketType"`
	Line       *float64          `json:"line"`
	Sport      string            `json:"sport"`
	Live       bool              `json:"live"`
	CreatedAt  int64             `json:"createdAt"`
	State      string            `json:"state"`
	Voidable   bool              `json:"voidable"`
	Status     string            `json:"status"`
	Booked     *float64          `json:"booked"`
	Profit     *float64          `json:"profit"`
	Legs       []bankrollLegView `json:"legs"`
}

// viewOf is o as the orders list shows it at now: its ledger legs and, for an order that needs a check, its other
// stored legs (no row yet).
func (o *ledgerOrder) viewOf(now int64) (bankrollOrderView, orderEval) {
	e := o.eval(now)
	v := bankrollOrderView{OrderID: o.row.ID, Source: rowSource(o.row.Source), Match: o.info.Match, Market: o.info.Market,
		MarketType: o.info.MarketType, Line: o.info.Line, Sport: o.info.Sport, Live: o.info.IsLive || o.info.Live, CreatedAt: o.row.CreatedAt,
		State: o.row.State, Voidable: e.voidable, Status: e.status, Booked: e.booked, Profit: e.profit, Legs: []bankrollLegView{}}
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

// handleBankrollOrders is GET /api/me/bot/bankroll/orders?source=app|auto&bookmaker=&status=&from=&to=&limit=&before=:
// the ledger orders (and the live orders that need a check), newest first (created_at, then id). bookmaker: a leg at
// that book; status: the order's status, except open = an order with a real leg still open; from/to: created_at in
// [from, to) (Unix ms); before: the cursor "<createdAt>.<orderId>" of the last order of the previous page.
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
	orders, err := b.loadLedger(userID)
	if err != nil {
		log.Error().Err(err).Msg("bot bankroll orders read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	now := b.now().UnixMilli()
	var views []bankrollOrderView
	for _, o := range orders {
		if len(o.legs) == 0 && !o.check {
			continue
		}
		v, e := o.viewOf(now)
		switch {
		case source != "" && v.Source != source,
			book != "" && !slices.ContainsFunc(v.Legs, func(l bankrollLegView) bool { return l.Bookmaker == book }),
			status == "open" && !slices.ContainsFunc(o.legs, func(l accounts.BotLedgerLeg) bool { return !legSettled(l) }),
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
// manual). payout is required (≥ 0) for cashed_out and ignored otherwise (computed from the leg's stake and odds);
// settledAt defaults to now and may be at most 5 min ahead; open resets the leg. A leg of the user's live order that
// needs a check and has no row yet gets one from its stored leg (placed at the order's end, else its send). 404
// not_found: no such leg of the user's; 400 bad_result: a bad body. The user's app sockets get {"type":"bot"}.
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

// setLedgerResult stores in on userID's ledger leg (orderID, leg), creating the row first for a live order that needs
// a check. false: no such leg. b.dbMu must be held.
func (b *botHub) setLedgerResult(userID int64, orderID string, leg int, in resultIn, now int64) (bool, error) {
	l, found, err := b.acc.BotLedgerLegOf(userID, orderID, leg)
	if err != nil {
		return false, err
	}
	if !found {
		row, ok, err := b.acc.BotOrderByID(userID, orderID)
		if err != nil || !ok || row.DryRun || !checkableOf(row.State, row.DryRun, row.FlowJSON) {
			return false, err
		}
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
