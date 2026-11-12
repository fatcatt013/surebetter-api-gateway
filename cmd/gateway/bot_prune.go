package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
)

// botRetention is how long bot log lines and screenshots are kept.
const botRetention = 7 * 24 * time.Hour

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
		if n, err := pruneFiles(b.shotsDir, cutoff); err != nil {
			log.Error().Err(err).Msg("bot screenshots prune failed")
		} else if n > 0 {
			log.Info().Int("deleted", n).Msg("old bot screenshots pruned")
		}
	}
}

// pruneFiles removes the regular files under dir modified before cutoff. A missing dir is nothing to do.
func pruneFiles(dir string, cutoff time.Time) (int, error) {
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
		if info.ModTime().Before(cutoff) && os.Remove(path) == nil {
			n++
		}
		return nil
	})
	return n, err
}

// runPrune prunes once now, then every hour until ctx ends.
func (b *botHub) runPrune(ctx context.Context) {
	b.prune()
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			b.prune()
		}
	}
}
