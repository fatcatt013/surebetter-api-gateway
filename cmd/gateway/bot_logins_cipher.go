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

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// loginKeyLen is the size of BOT_LOGINS_KEY after base64 decoding (AES-256).
const loginKeyLen = 32

// loginNonceLen is the GCM nonce at the start of every stored secret.
const loginNonceLen = 12

var (
	errBadLoginKey     = errors.New("not 32 bytes of standard base64")
	errUnknownLoginKey = errors.New("sealed with an unknown key")
	errLoginOpen       = errors.New("cannot decrypt")
)

// loginKey is one BOT_LOGINS_KEY: its AEAD, its id (lowercase hex of the first 8 bytes of SHA-256 of the key), the
// key of the user name tags (HMAC-SHA256(key, "bot-login user tag v1")) and the AEAD of the extension codes, under a
// key of its own (HMAC-SHA256(key, "extension code key v1")), so a login secret never opens as a code or back.
type loginKey struct {
	aead     cipher.AEAD
	id       string
	tagKey   []byte
	codeAEAD cipher.AEAD
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
	ck := hmac.New(sha256.New, raw)
	ck.Write([]byte("extension code key v1"))
	codeBlock, err := aes.NewCipher(ck.Sum(nil))
	if err != nil {
		return nil, errBadLoginKey
	}
	codeAEAD, err := cipher.NewGCM(codeBlock)
	if err != nil {
		return nil, errBadLoginKey
	}
	return &loginKey{aead: aead, id: hex.EncodeToString(sum[:8]), tagKey: m.Sum(nil), codeAEAD: codeAEAD}, nil
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

// codeAAD binds a sealed extension code to its row: extension_code:v1:<user_id>:<code_id>.
func codeAAD(userID, codeID int64) []byte {
	return []byte("extension_code:v1:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(codeID, 10))
}

// SealCode seals an extension code for its row with the current key's code AEAD: nonce ‖ AES-256-GCM
// (accounts.CodeCipher). The stored value carries no key id: OpenCode tries the current key, then the previous one.
func (c *loginCipher) SealCode(userID, codeID int64, code string) ([]byte, error) {
	nonce := make([]byte, loginNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.cur.codeAEAD.Seal(nonce, nonce, []byte(code), codeAAD(userID, codeID)), nil
}

// OpenCode opens a sealed extension code of its row with the current key, else the previous one (accounts.CodeCipher).
// Errors never hold the code.
func (c *loginCipher) OpenCode(userID, codeID int64, sealed []byte) (string, error) {
	code, _, err := c.openCodeKey(userID, codeID, sealed)
	return code, err
}

// openCodeKey is OpenCode that also tells which key opened it.
func (c *loginCipher) openCodeKey(userID, codeID int64, sealed []byte) (code string, k *loginKey, err error) {
	for _, k := range []*loginKey{c.cur, c.prev} {
		if k == nil || len(sealed) < loginNonceLen+k.codeAEAD.Overhead() {
			continue
		}
		plain, err := k.codeAEAD.Open(nil, sealed[:loginNonceLen], sealed[loginNonceLen:], codeAAD(userID, codeID))
		if err == nil {
			return string(plain), k, nil
		}
	}
	return "", nil, errLoginOpen
}

// resealCodes seals again with the current key every live extension code that opens only with the previous one (one
// transaction; the count is logged, never a code).
func (c *loginCipher) resealCodes(acc *accounts.Service) {
	done, err := acc.ResealCodes(func(sc accounts.SealedCode) ([]byte, bool) {
		code, k, err := c.openCodeKey(sc.UserID, sc.CodeID, sc.Sealed)
		if err != nil || k == c.cur {
			return nil, false
		}
		enc, err := c.SealCode(sc.UserID, sc.CodeID, code)
		return enc, err == nil
	})
	if err != nil {
		log.Error().Err(err).Msg("extension codes re-encryption failed; they stay as they are")
		return
	}
	log.Info().Int("count", done).Msg("extension codes re-encrypted")
}

// tag is the user name's tag under the current key: lowercase hex of HMAC-SHA256(tagKey, lower-cased user name).
func (c *loginCipher) tag(username string) string {
	m := hmac.New(sha256.New, c.cur.tagKey)
	m.Write([]byte(strings.ToLower(username)))
	return hex.EncodeToString(m.Sum(nil))
}

// setupBotLogins builds the cipher from cfg at start (nil = the feature is off, one error line) and, with a valid
// previous key, seals every row of that key again with the current one (one transaction; the count is logged).
// Neither key nor any login is ever logged.
func setupBotLogins(cfg Config, acc *accounts.Service) *loginCipher {
	if cfg.BotLoginsAllowHTTP {
		log.Warn().Msg("BOT_LOGINS_ALLOW_HTTP=1: bot logins are served over plain HTTP (development only)")
	}
	cur, err := parseLoginKey(cfg.BotLoginsKey)
	if err != nil {
		log.Error().Msg("bot logins off: BOT_LOGINS_KEY missing or not 32 bytes")
		return nil
	}
	c := &loginCipher{cur: cur}
	if strings.TrimSpace(cfg.BotLoginsKeyPrevious) != "" {
		prev, err := parseLoginKey(cfg.BotLoginsKeyPrevious)
		switch {
		case err != nil:
			log.Warn().Msg("BOT_LOGINS_KEY_PREVIOUS ignored: not 32 bytes of standard base64")
		case prev.id != cur.id:
			c.prev = prev
		}
	}
	if c.prev == nil || acc == nil {
		return c
	}
	c.resealCodes(acc)
	done, skipped, err := acc.RekeyBotLogins(c.prev.id, func(r accounts.BotLoginRow) ([]byte, string, string, error) {
		u, p, err := c.open(r.Secret, r.KeyID, r.UserID, r.CodeID, r.WindowID)
		if err != nil {
			return nil, "", "", err
		}
		return c.seal(r.UserID, r.CodeID, r.WindowID, u, p)
	})
	if err != nil {
		log.Error().Err(err).Msg("bot logins re-encryption failed; the previous key's rows stay as they are")
		return c
	}
	log.Info().Int("count", done).Msg("bot logins re-encrypted")
	if skipped > 0 {
		log.Warn().Int("count", skipped).Msg("bot logins not re-encrypted: they do not decrypt with BOT_LOGINS_KEY_PREVIOUS")
	}
	return c
}
