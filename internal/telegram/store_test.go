package telegram

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRedisStore(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return NewRedisStore(redis.NewClient(&redis.Options{Addr: mr.Addr()})), mr
}

func TestRedisStore_ConfigRoundTripAndMissing(t *testing.T) {
	s, _ := newRedisStore(t)
	ctx := context.Background()
	c, err := s.LoadConfig(ctx)
	if err != nil || c.Enabled {
		t.Fatalf("missing key → zero config, got %+v %v", c, err)
	}
	want := validConfig()
	want.Normalize()
	if err := s.SaveConfig(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadConfig(ctx)
	if err != nil || !got.Enabled || got.Preset.ID != "p1" || got.Bankroll.Total != 10000 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}

func TestRedisStore_Messages(t *testing.T) {
	s, mr := newRedisStore(t)
	ctx := context.Background()
	tr := Tracked{MessageID: 7, SentAt: 1, Arb: Arb{EventID: "e"}, PresetName: "p"}
	if err := s.SaveMessage(ctx, "k1", tr); err != nil {
		t.Fatal(err)
	}
	mr.HSet(MessagesKey, "junk", "{not json")
	got, err := s.LoadMessages(ctx)
	if err != nil || len(got) != 1 || got["k1"].MessageID != 7 || got["k1"].Arb.EventID != "e" {
		t.Fatalf("LoadMessages = %+v %v (junk entries are skipped)", got, err)
	}
	if err := s.DeleteMessage(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoadMessages(ctx); len(got) != 0 {
		t.Fatalf("after delete: %+v", got)
	}
}
