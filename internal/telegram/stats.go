package telegram

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"sync/atomic"

	"github.com/rs/zerolog/log"
)

// Statistics of the posted arbs: one SentArb per message the bot actually posted (edits add
// none), closed by one SentEnd when the bot learns the arb is gone. The gateway stores them in the
// accounts DB (telegram_sent_arbs, telegram_sent_legs). See surebetter-common/docs/telegram-stats.md.

// SentArb is one posted arb, with the money exactly as its message showed it.
type SentArb struct {
	MessageID       int64
	SentAt          int64 // ms, when the send succeeded
	UserID          int64 // the admin who last saved the bot config; 0 = unknown
	ArbKey          string
	FirstDetectedAt int64  // 0 = unknown
	Sport           string // bare, without "-live"
	IsLive          bool
	MarketType      string
	Line            float64
	ProfitPct       float64 // the engine's profit_pct
	ShownProfitPct  float64 // the % the message showed (after stake rounding)
	Currency        string
	TotalStake      int64
	Payout          int64 // guaranteed return: TotalStake + Profit
	Profit          int64
	Legs            []SentLeg
}

// SentLeg is one leg of a posted arb.
type SentLeg struct {
	Bookmaker string
	OutcomeID string
	Odds      float64
	Stake     int64
}

// SentEnd closes a posted arb's row. EndedAt 0 = the end time is unknown.
type SentEnd struct {
	MessageID int64
	EndedAt   int64
	Reason    string
}

// End reasons.
const (
	EndRemoved    = "removed"    // the engine removed the arb
	EndEvicted    = "evicted"    // the hub evicted it (no update for the eviction TTL); the end is late by up to the TTL
	EndRedetected = "redetected" // the engine re-detected it (new first_detected_at): the old one ended
	EndAbsent     = "absent"     // the 30 s reconcile found it gone from the hub (a dropped removal); late by up to 30 s
	EndUnknown    = "unknown"    // the bot lost track of it (restart, message deleted by hand): no end time
)

// Recorder receives the stats from the notifier's goroutine. It must never block.
type Recorder interface {
	RecordSent(SentArb)
	RecordEnd(SentEnd)
	// RecordSweep runs once at start: every open row whose message is not in keep (the messages
	// the bot still tracks) gets an unknown end.
	RecordSweep(keep []int64)
}

// StatsSink stores the stats (the accounts DB). Called on the StatsWriter's goroutine only.
type StatsSink interface {
	InsertSent(SentArb) error
	EndSent(SentEnd) error
	SweepSent(keep []int64) error
}

type statsOp struct {
	sent  *SentArb
	end   *SentEnd
	sweep []int64
}

// StatsWriter is the Recorder the gateway uses: a buffered queue drained by its own goroutine, so
// a slow or failing database never holds up a Telegram message. A full queue drops the record.
type StatsWriter struct {
	sink              StatsSink
	ops               chan statsOp
	errorsN, droppedN atomic.Int64
}

func NewStatsWriter(sink StatsSink, buffer int) *StatsWriter {
	return &StatsWriter{sink: sink, ops: make(chan statsOp, buffer)}
}

func (w *StatsWriter) push(op statsOp) {
	select {
	case w.ops <- op:
	default:
		w.droppedN.Add(1)
		log.Warn().Msg("telegram stats: queue full, record dropped")
	}
}

func (w *StatsWriter) RecordSent(s SentArb)     { w.push(statsOp{sent: &s}) }
func (w *StatsWriter) RecordEnd(e SentEnd)      { w.push(statsOp{end: &e}) }
func (w *StatsWriter) RecordSweep(keep []int64) { w.push(statsOp{sweep: append([]int64{}, keep...)}) }

// Run writes the queued records in order until ctx ends.
func (w *StatsWriter) Run(ctx context.Context) {
	for {
		select {
		case op := <-w.ops:
			var err error
			switch {
			case op.sent != nil:
				err = w.sink.InsertSent(*op.sent)
			case op.end != nil:
				err = w.sink.EndSent(*op.end)
			default:
				err = w.sink.SweepSent(op.sweep)
			}
			if err != nil {
				w.errorsN.Add(1)
				log.Warn().Err(err).Msg("telegram stats: write failed")
			}
		case <-ctx.Done():
			return
		}
	}
}

func (w *StatsWriter) Errors() int64  { return w.errorsN.Load() }
func (w *StatsWriter) Dropped() int64 { return w.droppedN.Load() }

// WriteMetrics appends the writer's counters to the gateway's /metrics.
func (w *StatsWriter) WriteMetrics(out io.Writer) {
	fmt.Fprintf(out, "gateway_telegram_stats_errors_total %d\ngateway_telegram_stats_dropped_total %d\n", w.errorsN.Load(), w.droppedN.Load())
}

// sentArb builds the stats row of a message posted for a (the arb as rendered) with bankroll b.
func sentArb(key string, a Arb, b Bankroll, userID, messageID, sentAt int64) SentArb {
	plan := ComputeStakes(a, b)
	whole := func(x float64) int64 { return int64(math.Round(x)) }
	legs := make([]SentLeg, len(a.Stakes))
	for i, s := range a.Stakes {
		legs[i] = SentLeg{Bookmaker: s.Bookmaker, OutcomeID: s.OutcomeID, Odds: s.DecimalOdds, Stake: whole(plan.Stakes[i])}
	}
	return SentArb{
		MessageID: messageID, SentAt: sentAt, UserID: userID, ArbKey: key, FirstDetectedAt: a.firstSeen(),
		Sport: strings.TrimSuffix(a.Sport, liveSuffix), IsLive: IsLive(a.Sport), MarketType: a.MarketType, Line: a.Line,
		ProfitPct: a.ProfitPct, ShownProfitPct: plan.ROI, Currency: b.Currency,
		TotalStake: whole(plan.Total), Payout: whole(plan.Total + plan.Profit), Profit: whole(plan.Profit), Legs: legs,
	}
}
