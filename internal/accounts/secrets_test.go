package accounts

import (
	"regexp"
	"testing"
)

func TestCodes_Format(t *testing.T) {
	short := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)
	ext := regexp.MustCompile(`^SBX(-[0-9A-HJKMNP-TV-Z]{4}){5}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := newShortCode()
		if !short.MatchString(c) {
			t.Fatalf("short code %q has the wrong format", c)
		}
		if seen[c] {
			t.Fatalf("duplicate short code %q", c)
		}
		seen[c] = true
		if e := newExtensionCode(); !ext.MatchString(e) {
			t.Fatalf("extension code %q has the wrong format", e)
		}
	}
	if tok := newSessionToken(); len(tok) != 64 {
		t.Fatalf("session token length = %d, want 64 hex chars", len(tok))
	}
}

func TestCodes_NormaliseHashHint(t *testing.T) {
	if got := normalizeCode("  sbx-ab12 -cd34\n"); got != "SBX-AB12-CD34" {
		t.Errorf("normalizeCode = %q", got)
	}
	if hashSecret("A") == hashSecret("B") || len(hashSecret("A")) != 64 {
		t.Error("hashSecret must be a 64-char SHA-256 hex digest that differs per input")
	}
	if got := hint("K7QX-2M9P-H4TD"); got != "H4TD" {
		t.Errorf("hint = %q, want H4TD", got)
	}
}
