package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// botRetention is how long bot log lines and screenshots are kept.
const botRetention = 7 * 24 * time.Hour

// orderSweepEvery is the scheduler's beat: waiting orders are retried and expired orders ended
// without waiting for a request (a var so a test can shorten it).
var orderSweepEvery = time.Second

// shotTmpMaxAge is how old a screenshot's temporary file must be for prune to treat it as left over.
const shotTmpMaxAge = 10 * time.Minute

// prune deletes log lines (by their ts) and screenshot files (by modification time) older than
// botRetention, and forgets ended rate windows.
func (b *botHub) prune() {
	now := b.now()
	cutoff := now.Add(-botRetention)
	b.logs.prune(now)
	b.shots.prune(now)
	if b.acc != nil {
		if n, err := b.acc.PruneBotLogs(cutoff.UnixMilli()); err != nil {
			log.Error().Err(err).Msg("bot logs prune failed")
		} else if n > 0 {
			log.Info().Int64("deleted", n).Msg("old bot log lines pruned")
		}
	}
	if b.shotsDir != "" {
		if n, err := pruneFiles(b.shotsDir, cutoff, now.Add(-shotTmpMaxAge)); err != nil {
			log.Error().Err(err).Msg("bot screenshots prune failed")
		} else if n > 0 {
			log.Info().Int("deleted", n).Msg("old bot screenshots pruned")
		}
	}
}

// pruneFiles removes the regular files under dir modified before cutoff, and the .tmp files (left
// by a write that failed or was interrupted) modified before tmpCutoff. A missing dir is nothing to do.
func pruneFiles(dir string, cutoff, tmpCutoff time.Time) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // removed meanwhile
		}
		limit := cutoff
		if strings.HasSuffix(path, ".tmp") {
			limit = tmpCutoff
		}
		if info.ModTime().Before(limit) && os.Remove(path) == nil {
			n++
		}
		return nil
	})
	return n, err
}

// runPrune prunes once now, then every hour, and runs the scheduler's tick every orderSweepEvery,
// until ctx ends.
func (b *botHub) runPrune(ctx context.Context) {
	b.prune()
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	sweep := time.NewTicker(orderSweepEvery)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			b.prune()
		case <-sweep.C:
			b.tick()
		}
	}
}
