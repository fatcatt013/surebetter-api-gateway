package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeUpdates struct {
	mu      sync.Mutex
	errs    []error // popped first, one per call; nil = answer from batches
	batches [][]Update
	offsets []int64
	me      string
	meErr   error
	meCalls int
}

func (f *fakeUpdates) GetUpdates(_ context.Context, offset int64, _ int) ([]Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offsets = append(f.offsets, offset)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(f.batches) == 0 {
		return nil, nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b, nil
}

func (f *fakeUpdates) GetMe(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meCalls++
	return f.me, f.meErr
}

type linkRec struct {
	chatID int64
}

type memLinks struct{ links map[int64]linkRec }

func (m *memLinks) SetTelegramLink(userID, chatID int64) error {
	m.links[userID] = linkRec{chatID}
	return nil
}

func newTestLinker() (*Linker, *fakeUpdates, *fakeOut, *memLinks, *fakeClock) {
	up := &fakeUpdates{me: "sb_alerts_bot"}
	out := &fakeOut{}
	store := &memLinks{links: map[int64]linkRec{}}
	clk := newFakeClock()
	l := NewLinker(up, out, store)
	l.now = clk.now
	return l, up, out, store, clk
}

func private(id int64, chat int64, text string) Update {
	return Update{UpdateID: id, Message: &Message{Chat: Chat{ID: chat, Type: "private"}, Text: text}}
}

func replies(out *fakeOut) []string {
	var texts []string
	for _, j := range out.take() {
		if j.Kind != JobAlert && j.Kind != JobReply {
			continue
		}
		texts = append(texts, j.Text)
	}
	return texts
}

func TestLinker_StartWithAValidCodeLinksOnce(t *testing.T) {
	l, _, out, store, _ := newTestLinker()
	code, exp := l.NewCode(7)
	if len(code) != 32 || exp.Sub(newFakeClock().now()) != LinkCodeTTL {
		t.Fatalf("code %q expires %v", code, exp)
	}
	l.handle(private(1, 555, "/start "+code))
	if got := store.links[7]; got.chatID != 555 {
		t.Fatalf("link: %+v", store.links)
	}
	jobs := out.take()
	if len(jobs) != 1 || jobs[0].Kind != JobAlert || jobs[0].ChatID != 555 || jobs[0].Text != "Connected: SureBetter bot alerts will come here." {
		t.Fatalf("reply: %+v", jobs)
	}
	l.handle(private(2, 999, "/start "+code)) // one time only
	if _, ok := store.links[7]; !ok || store.links[7].chatID != 555 {
		t.Fatalf("the code linked again: %+v", store.links)
	}
	if got := replies(out); len(got) != 1 || got[0] != badCodeText {
		t.Fatalf("reply to a used code: %v", got)
	}
}

func TestLinker_WrongExpiredAndReplacedCodes(t *testing.T) {
	l, _, out, store, clk := newTestLinker()
	l.handle(private(1, 555, "/start nope"))
	old, _ := l.NewCode(7)
	code, _ := l.NewCode(7) // a newer code replaces the user's older one
	l.handle(private(2, 556, "/start "+old))
	clk.sleep(context.Background(), LinkCodeTTL)
	l.handle(private(3, 557, "/start "+code))
	if len(store.links) != 0 {
		t.Fatalf("linked: %+v", store.links)
	}
	if got := replies(out); len(got) != 3 || got[0] != badCodeText || got[1] != badCodeText || got[2] != badCodeText {
		t.Fatalf("replies: %v", got)
	}
}

func TestLinker_GroupsIgnoredOtherMessagesAnswered(t *testing.T) {
	l, _, out, store, _ := newTestLinker()
	code, _ := l.NewCode(7)
	l.handle(Update{UpdateID: 1, Message: &Message{Chat: Chat{ID: -5306930735, Type: "group"}, Text: "/start " + code}})
	l.handle(Update{UpdateID: 2}) // no message
	if len(store.links) != 0 || len(out.take()) != 0 {
		t.Fatalf("a group message was handled: %+v", store.links)
	}
	l.handle(private(3, 555, "hello"))
	if got := replies(out); len(got) != 1 || got[0] != "This bot only sends SureBetter alerts." {
		t.Fatalf("reply: %v", got)
	}
	l.handle(private(4, 555, "/start "+code)) // the group message did not use it
	if store.links[7].chatID != 555 {
		t.Fatalf("link: %+v", store.links)
	}
}

func TestLinker_PollOnceMovesTheOffset(t *testing.T) {
	l, up, out, _, _ := newTestLinker()
	up.batches = [][]Update{{private(10, 555, "hi"), private(11, 556, "hi")}, {}}
	if err := l.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(up.offsets) != 2 || up.offsets[0] != 0 || up.offsets[1] != 12 {
		t.Fatalf("offsets %v, want [0 12]", up.offsets)
	}
	if got := replies(out); len(got) != 2 {
		t.Fatalf("replies %v", got)
	}
}

func TestLinker_BotUsernameIsAskedOnce(t *testing.T) {
	l, up, _, _, _ := newTestLinker()
	up.me, up.meErr = "", errors.New("down")
	if _, err := l.BotUsername(context.Background()); err == nil {
		t.Fatal("no error while getMe fails")
	}
	up.me, up.meErr = "sb_alerts_bot", nil
	for i := 0; i < 2; i++ {
		if u, err := l.BotUsername(context.Background()); err != nil || u != "sb_alerts_bot" {
			t.Fatalf("username %q %v", u, err)
		}
	}
	if up.meCalls != 2 {
		t.Fatalf("getMe calls = %d, want 2 (the failure, then one success kept)", up.meCalls)
	}
}

func TestLinker_RunStopsWithItsContext(t *testing.T) {
	l, _, _, _, _ := newTestLinker()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// Anyone can message the bot: one chat gets at most one answer per linkReplyEvery (as a low-priority JobReply); the
// answer to a valid /start <code> always goes, as a JobAlert.
func TestLinker_StrangersGetOneAnswerPerChatPer30Min(t *testing.T) {
	l, _, out, store, clk := newTestLinker()
	for i := 0; i < 100; i++ {
		l.handle(private(int64(i), 555, "spam"))
	}
	l.handle(private(200, 555, "/start nope"))
	l.handle(private(201, 555, "/start"))
	jobs := out.take()
	if len(jobs) != 1 || jobs[0].Kind != JobReply || jobs[0].ChatID != 555 || jobs[0].Text != onlyAlertsText {
		t.Fatalf("jobs: %+v", jobs)
	}
	code, _ := l.NewCode(7)
	l.handle(private(202, 555, "/start "+code))
	jobs = out.take()
	if len(jobs) != 1 || jobs[0].Kind != JobAlert || jobs[0].Text != linkedText || store.links[7].chatID != 555 {
		t.Fatalf("link answer: %+v", jobs)
	}
	clk.sleep(context.Background(), linkReplyEvery)
	l.handle(private(203, 555, "spam"))
	if got := out.take(); len(got) != 1 || got[0].Kind != JobReply {
		t.Fatalf("after 30 min: %+v", got)
	}
}

// A bare /start (someone opened the bot without a link) is not an expired link.
func TestLinker_BareStartIsNotAnExpiredLink(t *testing.T) {
	l, _, out, _, _ := newTestLinker()
	l.handle(private(1, 555, "/start"))
	l.handle(private(2, 556, "/start   "))
	if got := replies(out); len(got) != 2 || got[0] != onlyAlertsText || got[1] != onlyAlertsText {
		t.Fatalf("replies: %v", got)
	}
}

// A failing getUpdates waits 5 s, then twice as long each time up to 60 s; a success starts over.
func TestLinker_BackoffAfterFailures(t *testing.T) {
	l, up, _, _, _ := newTestLinker()
	boom := errors.New("down")
	up.errs = []error{boom, boom, boom, boom, boom, boom, nil, boom} // nil: one success
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	l.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 7 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	l.Run(ctx)
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second, 5 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits %v, want %v", waits, want)
		}
	}
}
