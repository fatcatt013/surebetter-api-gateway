package telegram

import "testing"

func TestArb_FirstSeen_PrefersFirstDetectedAt(t *testing.T) {
	if got := (Arb{FirstDetectedAt: 5, DetectedAt: 9}).firstSeen(); got != 5 {
		t.Fatalf("firstSeen = %d, want 5", got)
	}
	if got := (Arb{DetectedAt: 9}).firstSeen(); got != 9 {
		t.Fatalf("firstSeen fallback = %d, want 9", got)
	}
}
