package accounts

import (
	"strings"
	"testing"
)

func TestPassword_HashVerify(t *testing.T) {
	h, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected encoding %q", h)
	}
	if !verifyPassword("correct horse battery", h) {
		t.Error("the right password must verify")
	}
	if verifyPassword("correct horse batterY", h) {
		t.Error("a wrong password must not verify")
	}
	if verifyPassword("x", "garbage") {
		t.Error("a malformed hash must not verify")
	}
	h2, _ := hashPassword("correct horse battery")
	if h == h2 {
		t.Error("two hashes of one password must differ (random salt)")
	}
}

func TestPassword_Rules(t *testing.T) {
	if checkPassword("123456789") == nil {
		t.Error("9 characters must be rejected")
	}
	if checkPassword("1234567890") != nil {
		t.Error("10 characters must be accepted")
	}
	if checkPassword(strings.Repeat("a", 257)) == nil {
		t.Error("over 256 bytes must be rejected")
	}
}
