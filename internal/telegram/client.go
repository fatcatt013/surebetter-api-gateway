package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"
)

// Client is the slice of the Bot API the bot uses. Send, Edit and Delete go to the arbs chat (TELEGRAM_CHAT_ID);
// SendTo to one private chat (bot alerts and the linker's answers).
type Client interface {
	Send(ctx context.Context, text string) (int64, error)
	Edit(ctx context.Context, messageID int64, text string) error
	Delete(ctx context.Context, messageID int64) error
	SendTo(ctx context.Context, chatID int64, text string) (int64, error)
}

// APIError is a Bot API "ok": false response.
type APIError struct {
	Code            int
	Description     string
	RetryAfter      int   // seconds, on 429
	MigrateToChatID int64 // set when a group was upgraded to a supergroup
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("telegram %d: %s", e.Code, e.Description)
	if e.MigrateToChatID != 0 {
		s += fmt.Sprintf(" (new chat id %d: update TELEGRAM_CHAT_ID)", e.MigrateToChatID)
	}
	return s
}

// retryable: network failures and 5xx. 429 is handled separately (retry_after); other 4xx are permanent.
func retryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code >= 500
	}
	return true
}

func descContains(err error, sub string) bool {
	var ae *APIError
	return errors.As(err, &ae) && strings.Contains(ae.Description, sub)
}

// isMessageGone: the message was deleted from the chat by hand; stop tracking it.
func isMessageGone(err error) bool {
	return descContains(err, "message to edit not found") || descContains(err, "MESSAGE_ID_INVALID")
}

// NetError is a request that got no answer from Telegram. NotSent: it surely never reached Telegram (DNS failure, or
// no connection was made, also when the connect timed out), so sending it again cannot post a duplicate. A timeout or
// a broken connection once the connection was made is not NotSent: Telegram may have taken it.
type NetError struct {
	Msg     string
	NotSent bool
}

func (e *NetError) Error() string { return e.Msg }

// notReached: err happened before the request reached Telegram (a DNS failure or a dial error, a dial timeout
// included).
func notReached(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// HTTPClient talks to api.telegram.org.
type HTTPClient struct {
	BaseURL string
	Token   string
	ChatID  string
	HTTP    *http.Client
}

func NewHTTPClient(token, chatID string) *HTTPClient {
	return &HTTPClient{BaseURL: "https://api.telegram.org", Token: token, ChatID: chatID, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter      int   `json:"retry_after"`
		MigrateToChatID int64 `json:"migrate_to_chat_id"`
	} `json:"parameters"`
}

func (c *HTTPClient) call(ctx context.Context, method string, body map[string]any) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+c.Token+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return nil, c.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// gotConn: a connection to Telegram was made (new or reused). Without one the request cannot have reached it, also
	// when the client's timeout fired while connecting (that error is a context error, not a dial error).
	var gotConn atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) },
	}))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// *url.Error embeds the URL, which contains the token
		return nil, &NetError{Msg: c.redact(err).Error(), NotSent: !gotConn.Load() || notReached(err)}
	}
	defer resp.Body.Close()
	var r apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("telegram %s: HTTP %d with an undecodable body: %v", method, resp.StatusCode, err)
	}
	if !r.OK {
		code := r.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return nil, &APIError{Code: code, Description: r.Description, RetryAfter: r.Parameters.RetryAfter, MigrateToChatID: r.Parameters.MigrateToChatID}
	}
	return r.Result, nil
}

func (c *HTTPClient) redact(err error) error {
	if c.Token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.Token, "<token>"))
}

func (c *HTTPClient) base(text string) map[string]any {
	return map[string]any{
		"chat_id":              c.ChatID,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}
}

func (c *HTTPClient) Send(ctx context.Context, text string) (int64, error) {
	raw, err := c.call(ctx, "sendMessage", c.base(text))
	if err != nil {
		return 0, err
	}
	var m struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("telegram sendMessage: bad result: %v", err)
	}
	return m.MessageID, nil
}

func (c *HTTPClient) Edit(ctx context.Context, messageID int64, text string) error {
	body := c.base(text)
	body["message_id"] = messageID
	_, err := c.call(ctx, "editMessageText", body)
	if descContains(err, "message is not modified") {
		return nil
	}
	return err
}

// Delete removes a message; one already deleted by hand counts as done.
func (c *HTTPClient) Delete(ctx context.Context, messageID int64) error {
	_, err := c.call(ctx, "deleteMessage", map[string]any{"chat_id": c.ChatID, "message_id": messageID})
	if descContains(err, "message to delete not found") {
		return nil
	}
	return err
}

// SendTo sends plain text (no parse mode, no link preview) to chatID: bot alerts and the linker's answers.
func (c *HTTPClient) SendTo(ctx context.Context, chatID int64, text string) (int64, error) {
	raw, err := c.call(ctx, "sendMessage", map[string]any{
		"chat_id":              chatID,
		"text":                 text,
		"link_preview_options": map[string]any{"is_disabled": true},
	})
	if err != nil {
		return 0, err
	}
	var m struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("telegram sendMessage: bad result: %v", err)
	}
	return m.MessageID, nil
}

// Update is one incoming update; only messages are asked for.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is an incoming message: its chat and text.
type Message struct {
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
}

// Chat is where a message came from.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // private, group, supergroup or channel
}

// GetUpdates long-polls the bot's incoming messages after offset, waiting up to timeoutS seconds (keep it below the
// client's 15 s HTTP timeout).
func (c *HTTPClient) GetUpdates(ctx context.Context, offset int64, timeoutS int) ([]Update, error) {
	raw, err := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": timeoutS, "allowed_updates": []string{"message"}})
	if err != nil {
		return nil, err
	}
	var ups []Update
	if err := json.Unmarshal(raw, &ups); err != nil {
		return nil, fmt.Errorf("telegram getUpdates: bad result: %v", err)
	}
	return ups, nil
}

// GetMe returns the bot's username (for its t.me links).
func (c *HTTPClient) GetMe(ctx context.Context) (string, error) {
	raw, err := c.call(ctx, "getMe", map[string]any{})
	if err != nil {
		return "", err
	}
	var u struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", fmt.Errorf("telegram getMe: bad result: %v", err)
	}
	return u.Username, nil
}
