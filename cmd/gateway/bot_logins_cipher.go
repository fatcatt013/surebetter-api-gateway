package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// loginKeyLen is the size of BOT_LOGINS_KEY after base64 decoding (AES-256).
const loginKeyLen = 32

// loginNonceLen is the GCM nonce at the start of every stored secret.
const loginNonceLen = 12

var (
	errBadLoginKey    = errors.New("not 32 bytes of standard base64")
	errUnknownLoginKey = errors.New("sealed with an unknown key")
	errLoginOpen      = errors.New("cannot decrypt")
)

// loginKey is one BOT_LOGINS_KEY: its AEAD, its id (lowercase hex of the first 8 bytes of SHA-256 of the key) and the
// key of the user name tags (HMAC-SHA256(key, "bot-login user tag v1")).
type loginKey struct {
	aead   cipher.AEAD
	id     string
	tagKey []byte
}

// parseLoginKey reads a key in standard base64; anything that is not exactly 32 bytes after decoding is refused. The
// error never holds the value.
func parseLoginKey(s string) (*loginKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != loginKeyLen {
		return nil, errBadLoginKey
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, errBadLoginKey
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errBadLoginKey
	}
	sum := sha256.Sum256(raw)
	m := hmac.New(sha256.New, raw)
	m.Write([]byte("bot-login user tag v1"))
	return &loginKey{aead: aead, id: hex.EncodeToString(sum[:8]), tagKey: m.Sum(nil)}, nil
}

// loginCipher seals and opens the bot logins (contract §3). cur is BOT_LOGINS_KEY; prev, when set, is a valid
// BOT_LOGINS_KEY_PREVIOUS (used to re-encrypt at start and to open a row not re-encrypted yet).
type loginCipher struct {
	cur  *loginKey
	prev *loginKey
}

// loginPlain is the sealed plaintext: {"u":"<user name>","p":"<password>"}.
type loginPlain struct {
	U string `json:"u"`
	P string `json:"p"`
}

// loginAAD binds a secret to its row: bot-login:v1:<user_id>:<code_id>:<window_id>.
func loginAAD(userID, codeID int64, windowID string) []byte {
	return []byte("bot-login:v1:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(codeID, 10) + ":" + windowID)
}

// seal encrypts a login for its row with the current key: nonce ‖ AES-256-GCM, the key id and the user name's tag.
func (c *loginCipher) seal(userID, codeID int64, windowID, username, password string) (secret []byte, keyID, tag string, err error) {
	plain, err := json.Marshal(loginPlain{U: username, P: password})
	if err != nil {
		return nil, "", "", err
	}
	nonce := make([]byte, loginNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", "", err
	}
	secret = c.cur.aead.Seal(nonce, nonce, plain, loginAAD(userID, codeID, windowID))
	clear(plain)
	return secret, c.cur.id, c.tag(username), nil
}

// keyOf is the key with id: the current or the previous one; nil = a lost key.
func (c *loginCipher) keyOf(id string) *loginKey {
	switch {
	case c.cur != nil && c.cur.id == id:
		return c.cur
	case c.prev != nil && c.prev.id == id:
		return c.prev
	}
	return nil
}

// known: a row sealed with keyID can be opened (it is not key_lost).
func (c *loginCipher) known(keyID string) bool { return c.keyOf(keyID) != nil }

// open decrypts a row's secret. Errors never hold the plaintext.
func (c *loginCipher) open(secret []byte, keyID string, userID, codeID int64, windowID string) (username, password string, err error) {
	k := c.keyOf(keyID)
	if k == nil {
		return "", "", errUnknownLoginKey
	}
	if len(secret) < loginNonceLen+k.aead.Overhead() {
		return "", "", errLoginOpen
	}
	plain, err := k.aead.Open(nil, secret[:loginNonceLen], secret[loginNonceLen:], loginAAD(userID, codeID, windowID))
	if err != nil {
		return "", "", errLoginOpen
	}
	var p loginPlain
	err = json.Unmarshal(plain, &p)
	clear(plain)
	if err != nil {
		return "", "", errLoginOpen
	}
	return p.U, p.P, nil
}

// tag is the user name's tag under the current key: lowercase hex of HMAC-SHA256(tagKey, lower-cased user name).
func (c *loginCipher) tag(username string) string {
	m := hmac.New(sha256.New, c.cur.tagKey)
	m.Write([]byte(strings.ToLower(username)))
	return hex.EncodeToString(m.Sum(nil))
}
