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

// PinMessage pins a message in the conversation (messages.pin), so the game
// announcement with the roster stays at the top of the chat instead of
// scrolling away. VK allows this for chat administrators; when it is not
// allowed the caller logs it and the announcement still works.
func (c *Client) PinMessage(ctx context.Context, peerID, messageID int64) error {
	params := url.Values{}
	params.Set("peer_id", strconv.FormatInt(peerID, 10))
	params.Set("message_id", strconv.FormatInt(messageID, 10))

	raw, err := c.call(ctx, "messages.pin", params)
	if err != nil {
		return err
	}
	var ok int
	if err := json.Unmarshal(raw, &ok); err != nil {
		return fmt.Errorf("parse messages.pin response: %w", err)
	}
	if ok != 1 {
		return fmt.Errorf("messages.pin: unexpected response %d", ok)
	}
	return nil
}

// EditMessage edits a message the community sent earlier (messages.edit).
// This is how the game announcement keeps the roster in one message instead
// of posting a new one on every booking. It returns the edited message id.
func (c *Client) EditMessage(ctx context.Context, peerID, messageID int64, text, keyboard string) (int64, error) {
	params := url.Values{}
	params.Set("peer_id", strconv.FormatInt(peerID, 10))
	params.Set("message_id", strconv.FormatInt(messageID, 10))
	params.Set("message", text)
	if keyboard != "" {
		params.Set("keyboard", keyboard)
	}

	raw, err := c.call(ctx, "messages.edit", params)
	if err != nil {
		return 0, err
	}
	var ok int
	if err := json.Unmarshal(raw, &ok); err != nil {
		return 0, fmt.Errorf("parse messages.edit response: %w", err)
	}
	if ok != 1 {
		return 0, fmt.Errorf("messages.edit: unexpected response %d", ok)
	}
	return messageID, nil
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

// vkFlag is a VK boolean flag. Depending on the method and API version VK
// returns these either as JSON booleans (the messages.getConversationMembers
// example in the docs uses "is_admin": true) or as 0/1 integers, so both
// forms must parse. Anything else is an error rather than a silent false:
// a wrong type would otherwise be reported as "not an admin".
type vkFlag bool

func (f *vkFlag) UnmarshalJSON(data []byte) error {
	switch strings.TrimSpace(string(data)) {
	case "true", "1":
		*f = true
	case "false", "0", "null":
		*f = false
	default:
		return fmt.Errorf("vkFlag: unexpected value %s", data)
	}
	return nil
}

// GetConversationTitle returns the title of a group conversation
// (messages.getConversationsById). It fails when VK returns no title, e.g.
// for a one-to-one dialog or when the bot cannot read the conversation.
func (c *Client) GetConversationTitle(ctx context.Context, peerID int64) (string, error) {
	params := url.Values{}
	params.Set("peer_ids", strconv.FormatInt(peerID, 10))

	raw, err := c.call(ctx, "messages.getConversationsById", params)
	if err != nil {
		return "", err
	}
	var resp struct {
		Items []struct {
			ChatSettings struct {
				Title string `json:"title"`
			} `json:"chat_settings"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse messages.getConversationsById response: %w", err)
	}
	if len(resp.Items) == 0 {
		return "", fmt.Errorf("messages.getConversationsById: no items for peer %d", peerID)
	}
	title := strings.TrimSpace(resp.Items[0].ChatSettings.Title)
	if title == "" {
		return "", fmt.Errorf("messages.getConversationsById: empty title for peer %d", peerID)
	}
	return title, nil
}

// IsConversationAdmin reports whether userID administers the conversation
// (messages.getConversationMembers). VK marks both an administrator and the
// owner; both count as admins here. The members list is paginated, but the
// initiator of the connect command is an active participant and is returned
// on the first page, so V1 does not page through it.
//
// In a one-to-one dialog the method returns members without is_admin/is_owner
// at all, so a dialog can never pass this check.
func (c *Client) IsConversationAdmin(ctx context.Context, peerID, userID int64) (bool, error) {
	params := url.Values{}
	params.Set("peer_id", strconv.FormatInt(peerID, 10))

	raw, err := c.call(ctx, "messages.getConversationMembers", params)
	if err != nil {
		return false, err
	}
	var resp struct {
		Items []struct {
			MemberID int64  `json:"member_id"`
			IsAdmin  vkFlag `json:"is_admin"`
			IsOwner  vkFlag `json:"is_owner"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return false, fmt.Errorf("parse messages.getConversationMembers response: %w", err)
	}
	for _, m := range resp.Items {
		if m.MemberID == userID {
			return bool(m.IsAdmin) || bool(m.IsOwner), nil
		}
	}
	return false, nil
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
