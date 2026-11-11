package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// crockford is Crockford's base32 alphabet (no I, L, O, U), so codes are easy to read and type.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// randomGroups returns n groups of 4 random base32 characters joined by "-" (5 bits each).
func randomGroups(n int) string {
	b := make([]byte, n*4)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	var sb strings.Builder
	for i, x := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(crockford[x&31]) // 256 is a multiple of 32: uniform
	}
	return sb.String()
}

// newShortCode is an invite or password-reset code: XXXX-XXXX-XXXX, 60 bits.
func newShortCode() string { return randomGroups(3) }

// newExtensionCode is a long-lived extension credential: SBX- plus 5 groups, 100 bits.
func newExtensionCode() string { return "SBX-" + randomGroups(5) }

// newSessionToken is a session token or WebSocket ticket: 32 random bytes as hex.
func newSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// normalizeCode makes pasted codes comparable: upper case, all whitespace removed.
func normalizeCode(s string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s))
}

// hashSecret is what the database stores for every token and code. Secrets are random with
// 60+ bits, so a fast hash is enough (no password hashing needed).
func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hint is the last 4 characters of a code, shown in lists to tell codes apart.
func hint(code string) string {
	if len(code) <= 4 {
		return code
	}
	return code[len(code)-4:]
}
