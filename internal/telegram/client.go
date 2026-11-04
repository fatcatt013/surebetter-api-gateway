package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client is the slice of the Bot API the bot uses.
type Client interface {
	Send(ctx context.Context, text string) (int64, error)
	Edit(ctx context.Context, messageID int64, text string) error
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
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, c.redact(err) // *url.Error embeds the URL, which contains the token
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
