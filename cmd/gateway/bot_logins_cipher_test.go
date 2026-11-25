package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// testLoginKey is a fixed 32-byte key in base64 (seed 1..32 + n).
func testLoginKey(n byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i) + 1 + n
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func mustCipher(t *testing.T, cur, prev string) *loginCipher {
	t.Helper()
	c, err := parseLoginKey(cur)
	if err != nil {
		t.Fatal(err)
	}
	lc := &loginCipher{cur: c}
	if prev != "" {
		if lc.prev, err = parseLoginKey(prev); err != nil {
			t.Fatal(err)
		}
	}
	return lc
}

func TestLoginCipher_RoundTripAndAAD(t *testing.T) {
	c := mustCipher(t, testLoginKey(0), "")
	secret, keyID, tag, err := c.seal(7, 12, "tipsport-1", "Jan.Novák", "pÁss wörd!1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(secret, []byte("Jan.Novák")) || bytes.Contains(secret, []byte("pÁss")) {
		t.Fatal("plaintext in the secret")
	}
	if keyID != c.cur.id || tag != c.tag("jan.novák") {
		t.Fatalf("key id %s tag %s", keyID, tag)
	}
	u, p, err := c.open(secret, keyID, 7, 12, "tipsport-1")
	if err != nil || u != "Jan.Novák" || p != "pÁss wörd!1" {
		t.Fatalf("open: %q %q %v", u, p, err)
	}
	// A secret moved to another row (user, code or window) does not open.
	for _, row := range []struct {
		u, c int64
		w    string
	}{{8, 12, "tipsport-1"}, {7, 13, "tipsport-1"}, {7, 12, "tipsport-2"}} {
		if _, _, err := c.open(secret, keyID, row.u, row.c, row.w); err == nil {
			t.Fatalf("opened on another row %+v", row)
		}
	}
	// Two seals of the same login differ (a new nonce each time).
	again, _, _, _ := c.seal(7, 12, "tipsport-1", "Jan.Novák", "pÁss wörd!1")
	if bytes.Equal(again[:loginNonceLen], secret[:loginNonceLen]) {
		t.Fatal("nonce reused")
	}
	// Truncated or tampered secrets fail, without the plaintext in the error.
	tampered := bytes.Clone(secret)
	tampered[len(tampered)-1] ^= 1
	for _, s := range [][]byte{nil, secret[:5], tampered} {
		if _, _, err := c.open(s, keyID, 7, 12, "tipsport-1"); err == nil {
			t.Fatal("bad secret opened")
		}
	}
	if _, _, err := c.open(secret, "0000000000000000", 7, 12, "tipsport-1"); err == nil || c.known("0000000000000000") {
		t.Fatal("unknown key id opened")
	}
}

func TestLoginCipher_KeyIDAndTag(t *testing.T) {
	raw, _ := base64.StdEncoding.DecodeString(testLoginKey(0))
	sum := sha256.Sum256(raw)
	c := mustCipher(t, testLoginKey(0), testLoginKey(1))
	if c.cur.id != hex.EncodeToString(sum[:8]) || len(c.cur.id) != 16 {
		t.Fatalf("key id %s", c.cur.id)
	}
	if !c.known(c.cur.id) || !c.known(c.prev.id) || c.cur.id == c.prev.id {
		t.Fatal("known ids")
	}
	// Same key → same tag for the same user name, whatever its case; another key → another tag.
	if c.tag("Jan") != c.tag("jAN") || c.tag("jan") == c.tag("jana") {
		t.Fatal("tag per user name")
	}
	other := mustCipher(t, testLoginKey(1), "")
	if other.tag("jan") == c.tag("jan") || len(c.tag("jan")) != 64 {
		t.Fatal("tag per key")
	}
	// The previous key opens what it sealed.
	secret, keyID, _, _ := other.seal(1, 2, "betano-1", "u", "p")
	if u, p, err := c.open(secret, keyID, 1, 2, "betano-1"); err != nil || u != "u" || p != "p" {
		t.Fatalf("previous key: %v", err)
	}
}

func TestParseLoginKey_Refuses(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(make([]byte, 31))
	long := base64.StdEncoding.EncodeToString(make([]byte, 33))
	for _, s := range []string{"", "not base64!!", short, long, base64.RawStdEncoding.EncodeToString(make([]byte, 32)) + "%"} {
		if _, err := parseLoginKey(s); err == nil {
			t.Fatalf("accepted %q", s)
		} else if s != "" && bytes.Contains([]byte(err.Error()), []byte(s)) {
			t.Fatal("the key in the error")
		}
	}
	if _, err := parseLoginKey(" " + testLoginKey(3) + "\n"); err != nil {
		t.Fatal("surrounding white space refused")
	}
}
