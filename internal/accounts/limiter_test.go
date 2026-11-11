package accounts

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)} }

func TestLimiter_LocksAfterMaxFailuresInWindow(t *testing.T) {
	clk := newClock()
	l := newLimiter(5, 15*time.Minute, 15*time.Minute, clk.now)
	for i := 0; i < 4; i++ {
		l.fail("k")
	}
	if ok, _ := l.check("k"); !ok {
		t.Fatal("4 failures must not lock")
	}
	l.fail("k")
	ok, wait := l.check("k")
	if ok || wait != 15*time.Minute {
		t.Fatalf("5th failure must lock for 15m, got ok=%v wait=%v", ok, wait)
	}
	clk.advance(15 * time.Minute)
	if ok, _ := l.check("k"); !ok {
		t.Fatal("the lock must end after 15 minutes")
	}
	if ok, _ := l.check("other"); !ok {
		t.Fatal("other keys are independent")
	}
}

func TestLimiter_OldFailuresExpireAndResetClears(t *testing.T) {
	clk := newClock()
	l := newLimiter(5, 15*time.Minute, 15*time.Minute, clk.now)
	for i := 0; i < 4; i++ {
		l.fail("k")
	}
	clk.advance(16 * time.Minute) // those 4 fall out of the window
	l.fail("k")
	if ok, _ := l.check("k"); !ok {
		t.Fatal("failures outside the window must not count")
	}
	for i := 0; i < 3; i++ {
		l.fail("k")
	}
	l.reset("k")
	l.fail("k")
	if ok, _ := l.check("k"); !ok {
		t.Fatal("reset must clear the count")
	}
}
