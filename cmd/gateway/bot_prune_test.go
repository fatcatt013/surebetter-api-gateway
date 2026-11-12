package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func TestBotPrune_DropsLogsAndScreenshotsOlderThanSevenDays(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	_, codeID := newBotCode(t, srv, token, "Home PC")

	old := clk.now().Add(-botRetention - time.Minute)
	fresh := clk.now().Add(-botRetention + time.Minute)
	// Stored directly: POST /api/me/bot/logs refuses a ts over 7 days old.
	if err := svc.AddBotLogs(userIDOf(t, svc, token), codeID, []accounts.BotLogLine{
		{TS: old.UnixMilli(), Level: "info", Msg: "old line"}, {TS: fresh.UnixMilli(), Level: "info", Msg: "fresh line"},
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(bots.shotsDir, fmt.Sprint(userIDOf(t, svc, token)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, mtime := range map[string]time.Time{"aaaaaaaaaaaaaaaa.jpg": old, "bbbbbbbbbbbbbbbb.jpg": fresh} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, jpeg(10), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bots.runPrune(ctx) // prunes once at start, then returns: ctx is done

	if got := msgs(getLogs(t, srv, token, "")); !slices.Equal(got, []string{"fresh line"}) {
		t.Fatalf("logs after prune: %v", got)
	}
	left, _ := os.ReadDir(dir)
	names := []string{}
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "bbbbbbbbbbbbbbbb.jpg" {
		t.Fatalf("screenshots after prune: %v", names)
	}
}

func TestBotPrune_NoScreenshotsDirYet(t *testing.T) {
	bots := newBotHub(newConnRegistry(), nil)
	bots.shotsDir = filepath.Join(t.TempDir(), "missing")
	bots.prune() // must not fail or create anything
	if _, err := os.Stat(bots.shotsDir); !os.IsNotExist(err) {
		t.Fatalf("prune created %s: %v", bots.shotsDir, err)
	}
}
