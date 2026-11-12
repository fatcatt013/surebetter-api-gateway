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

// A .tmp file a failed or interrupted screenshot write left behind is removed by the next prune once
// it is older than shotTmpMaxAge; a write in progress (a fresh .tmp) is not touched.
func TestBotPrune_RemovesLeftoverTmpFiles(t *testing.T) {
	bots := newBotHub(newConnRegistry(), nil)
	clk := &botClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	bots.shotsDir = t.TempDir()
	dir := filepath.Join(bots.shotsDir, "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, age := range map[string]time.Duration{
		"aaaaaaaaaaaaaaaa.jpg.tmp": shotTmpMaxAge + time.Minute,
		"bbbbbbbbbbbbbbbb.jpg.tmp": time.Second,
		"cccccccccccccccc.jpg":     shotTmpMaxAge + time.Minute,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, jpeg(10), 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := clk.now().Add(-age)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	bots.prune()
	left, _ := os.ReadDir(dir)
	names := []string{}
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "bbbbbbbbbbbbbbbb.jpg.tmp,cccccccccccccccc.jpg" {
		t.Fatalf("after prune: %v", names)
	}
}

// An expired order releases its agent and the app is told, with no request to trigger it.
func TestBotPrune_ExpiredOrdersReleaseBusyOnTheirOwn(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	bot := startBot(t, srv, newCode(t, srv, token, "bot"), app)
	placeOrder(t, srv, token, bot, app)
	clk.advance(orderMemory)

	defer func(d time.Duration) { orderSweepEvery = d }(orderSweepEvery)
	orderSweepEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { bots.runPrune(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on the expired order: %s", got)
	}
	uid := userIDOf(t, svc, token)
	bots.mu.Lock()
	defer bots.mu.Unlock()
	for _, p := range bots.agents[uid] {
		if p.busy != "" {
			t.Fatalf("still busy with %s", p.busy)
		}
	}
}
