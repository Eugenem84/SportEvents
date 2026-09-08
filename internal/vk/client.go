package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// vkAPIVersion is the Callback API / method version this adapter is built
// against. Bump together with the community Callback API settings.
const vkAPIVersion = "5.199"

const defaultVKBaseURL = "https://api.vk.com/method"

// Client is a thin wrapper around the VK Bot API methods the adapter uses:
// messages.send and users.get. It is injectable into the callback service
// as a Messenger.
type Client struct {
	baseURL string
	token   string
	groupID int64
	http    *http.Client
}

// ClientOption customizes a Client (used by tests to point at a stub).
type ClientOption func(*Client)

// WithBaseURL overrides the VK API endpoint.
func WithBaseURL(baseURL string) ClientOption {
	return func(c *Client) { c.baseURL = strings.TrimRight(baseURL, "/") }
}

// WithHTTPClient overrides the underlying HTTP client.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) { c.http = hc }
}

func NewClient(token, groupID string, opts ...ClientOption) *Client {
	gid, _ := strconv.ParseInt(strings.TrimSpace(groupID), 10, 64)
	c := &Client{
		baseURL: defaultVKBaseURL,
		token:   token,
		groupID: gid,
		http:    &http.Client{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// SendMessage sends a text message to a peer (user or conversation) and
// returns the VK message id. random_id makes repeated sends of the same
// logical message idempotent within VK's dedup window.
func (c *Client) SendMessage(ctx context.Context, peerID int64, text, keyboard string) (int64, error) {
	params := url.Values{}
	params.Set("peer_id", strconv.FormatInt(peerID, 10))
	params.Set("message", text)
	params.Set("random_id", strconv.FormatInt(int64(rand.Int32()), 10))
	if keyboard != "" {
		params.Set("keyboard", keyboard)
	}

	raw, err := c.call(ctx, "messages.send", params)
	if err != nil {
		return 0, err
	}
	var msgID int64
	if err := json.Unmarshal(raw, &msgID); err != nil {
		return 0, fmt.Errorf("parse messages.send response: %w", err)
	}
	return msgID, nil
}

// AnswerMessageEvent acknowledges a callback-button press
// (messages.sendMessageEventAnswer) so the pressed button stops showing the
// loading state. eventData, when non-empty, is the JSON action to run on the
// client (e.g. a toast for showing the result).
func (c *Client) AnswerMessageEvent(ctx context.Context, userID, peerID int64, eventID, eventData string) error {
	params := url.Values{}
	params.Set("user_id", strconv.FormatInt(userID, 10))
	params.Set("peer_id", strconv.FormatInt(peerID, 10))
	params.Set("event_id", eventID)
	if eventData != "" {
		params.Set("event_data", eventData)
	}

	raw, err := c.call(ctx, "messages.sendMessageEventAnswer", params)
	if err != nil {
		return err
	}
	var ok int
	if err := json.Unmarshal(raw, &ok); err != nil {
		return fmt.Errorf("parse messages.sendMessageEventAnswer response: %w", err)
	}
	if ok != 1 {
		return fmt.Errorf("messages.sendMessageEventAnswer: unexpected response %d", ok)
	}
	return nil
}

// GetUserName returns "First Last" for a VK user id.
func (c *Client) GetUserName(ctx context.Context, userID int64) (string, error) {
	params := url.Values{}
	params.Set("user_ids", strconv.FormatInt(userID, 10))

	raw, err := c.call(ctx, "users.get", params)
	if err != nil {
		return "", err
	}
	var users []struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}
	if err := json.Unmarshal(raw, &users); err != nil {
		return "", fmt.Errorf("parse users.get response: %w", err)
	}
	if len(users) == 0 {
		return "", fmt.Errorf("users.get: empty response")
	}
	name := strings.TrimSpace(users[0].FirstName + " " + users[0].LastName)
	if name == "" {
		return "", fmt.Errorf("users.get: empty name for user %d", userID)
	}
	return name, nil
}

type apiResponse struct {
	Response json.RawMessage `json:"response"`
	Error    *apiError       `json:"error"`
}

type apiError struct {
	Code    int    `json:"error_code"`
	Message string `json:"error_msg"`
}

func (c *Client) call(ctx context.Context, method string, params url.Values) (json.RawMessage, error) {
	params.Set("access_token", c.token)
	params.Set("v", vkAPIVersion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/"+method, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, fmt.Errorf("vk %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vk %s: %w", method, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("vk %s: read response: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vk %s: status %d: %s", method, resp.StatusCode, truncate(body))
	}

	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("vk %s: parse response: %w", method, err)
	}
	if ar.Error != nil {
		return nil, fmt.Errorf("vk %s: error %d: %s", method, ar.Error.Code, ar.Error.Message)
	}
	return ar.Response, nil
}

func truncate(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
