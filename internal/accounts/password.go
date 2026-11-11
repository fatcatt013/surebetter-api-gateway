package accounts

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP minimum): 19 MiB, 2 passes, 1 lane.
const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32

	// MinPasswordLen is the shortest accepted password, in characters.
	MinPasswordLen = 10
)

func checkPassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLen || len(pw) > 256 {
		return ErrWeakPassword
	}
	return nil
}

// hashPassword returns a PHC-style string: $argon2id$v=19$m=…,t=…,p=…$salt$key (raw base64).
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argonKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	if t == 0 || p == 0 || m == 0 || m > 1<<20 { // argon2.IDKey panics on t or p of 0; cap memory at 1 GiB
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argonKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var (
	dummyOnce sync.Once
	dummy     string
)

// dummyHash is verified against when the email is unknown, so a login takes as long whether or
// not the account exists.
func dummyHash() string {
	dummyOnce.Do(func() { dummy, _ = hashPassword("timing equaliser, never a real password") })
	return dummy
}

// hashSlots caps concurrent argon2 runs (19 MiB each), so a burst of login or sign-up attempts
// queues instead of exhausting the host's memory.
var hashSlots = make(chan struct{}, 2)

func argonKey(pw, salt []byte, t, m uint32, p uint8, keyLen uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey(pw, salt, t, m, p, keyLen)
}
