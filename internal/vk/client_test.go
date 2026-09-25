package vk

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// These tests exercise internal/vk against a local stub of the VK HTTP API,
// so they need neither PostgreSQL nor real VK credentials.

func TestNewClientNilHTTP(t *testing.T) {
	c := NewClient("tok", "123")
	if c == nil {
		t.Fatal("client must not be nil")
	}
}

func TestSendMessagePostsFormParams(t *testing.T) {
	var gotBody string
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: %q", r.Method)
		}
		gotContentType = r.Header.Get("Content-Type")
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":42}`))
	}))
	defer srv.Close()

	c := NewClient("tok123", "123", WithBaseURL(srv.URL))
	msgID, err := c.SendMessage(context.Background(), 2_000_000_047, "Привет", `{"one_time":false}`)
	if err != nil {
		t.Fatal(err)
	}
	if msgID != 42 {
		t.Fatalf("message id: want 42, got %d", msgID)
	}
	if !strings.HasPrefix(gotContentType, "application/x-www-form-urlencoded") {
		t.Fatalf("content type: %q", gotContentType)
	}

	params, err := url.ParseQuery(gotBody)
	if err != nil {
		t.Fatal(err)
	}
	if params.Get("access_token") != "tok123" {
		t.Fatalf("access_token: %q", params.Get("access_token"))
	}
	if params.Get("peer_id") != "2000000047" {
		t.Fatalf("peer_id: %q", params.Get("peer_id"))
	}
	if params.Get("message") != "Привет" {
		t.Fatalf("message: %q", params.Get("message"))
	}
	if params.Get("keyboard") != `{"one_time":false}` {
		t.Fatalf("keyboard: %q", params.Get("keyboard"))
	}
	if params.Get("random_id") == "" {
		t.Fatal("random_id must be set for idempotent sends")
	}
	if params.Get("v") == "" {
		t.Fatal("v (api version) must be set")
	}
}

func TestSendMessageNoKeyboardOmitsParam(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		got = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":1}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	if _, err := c.SendMessage(context.Background(), 123, "ok", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "keyboard") {
		t.Fatalf("keyboard must be omitted, got %q", got)
	}
}

func TestSendMessageVKError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":{"error_code":911,"error_msg":"Keyboard format is invalid"}}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	_, err := c.SendMessage(context.Background(), 1, "x", "bad")
	if err == nil {
		t.Fatal("want error for VK error response")
	}
	if !strings.Contains(err.Error(), "911") {
		t.Fatalf("error must carry vk code, got %v", err)
	}
}

func TestSendMessageHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	if _, err := c.SendMessage(context.Background(), 1, "x", ""); err == nil {
		t.Fatal("want error for non-200 response")
	}
}

func TestGetUserNameParsesFullName(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		got = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":[{"id":1,"first_name":"Иван","last_name":"Петров"}]}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	name, err := c.GetUserName(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Иван Петров" {
		t.Fatalf("name: %q", name)
	}
	if !strings.Contains(got, "user_ids=1") {
		t.Fatalf("user_ids param missing: %q", got)
	}
}

func TestGetUserNameEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":[]}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	if _, err := c.GetUserName(context.Background(), 1); err == nil {
		t.Fatal("want error for empty users list")
	}
}

func TestCommandPayloadAndBack(t *testing.T) {
	p := CommandPayload("games")
	if !strings.Contains(p, `"command"`) || !strings.Contains(p, "games") {
		t.Fatalf("payload: %q", p)
	}
	if got := PayloadCommand(p); got != "games" {
		t.Fatalf("round trip: want games, got %q", got)
	}
	if got := PayloadCommand(`{"command":"start"}`); got != "start" {
		t.Fatalf("start payload: got %q", got)
	}
	if got := PayloadCommand("not json"); got != "" {
		t.Fatalf("garbage payload must yield empty command, got %q", got)
	}
}

func TestSendMessageEventAnswerPostsParams(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "messages.sendMessageEventAnswer") {
			t.Errorf("method path: %q", r.URL.Path)
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		got = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":1}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	err := c.AnswerMessageEvent(context.Background(), 555, 2_000_000_047, "evt-9", `{"type":"show_snackbar","text":"ok"}`)
	if err != nil {
		t.Fatal(err)
	}
	params, err := url.ParseQuery(got)
	if err != nil {
		t.Fatal(err)
	}
	if params.Get("user_id") != "555" {
		t.Fatalf("user_id: %q", params.Get("user_id"))
	}
	if params.Get("peer_id") != "2000000047" {
		t.Fatalf("peer_id: %q", params.Get("peer_id"))
	}
	if params.Get("event_id") != "evt-9" {
		t.Fatalf("event_id: %q", params.Get("event_id"))
	}
	if params.Get("event_data") == "" {
		t.Fatal("event_data must be set when provided")
	}
}

func TestSendMessageEventAnswerNoDataOmitsParam(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		got = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":1}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	if err := c.AnswerMessageEvent(context.Background(), 555, 2000000047, "evt-9", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "event_data") {
		t.Fatalf("event_data must be omitted, got %q", got)
	}
}

func TestSendMessageEventAnswerUnexpectedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":0}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	err := c.AnswerMessageEvent(context.Background(), 555, 1, "evt-9", "")
	if err == nil {
		t.Fatal("want error for unexpected response 0")
	}
}

func TestKeyboardMarshal(t *testing.T) {
	kb := Keyboard{
		Inline: true,
		Buttons: [][]Button{
			{TextButton("Игры", CommandPayload("games"), ColorPrimary)},
		},
	}
	raw, err := kb.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var parsed json.RawMessage
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("keyboard must be valid json: %v", err)
	}
	if !strings.Contains(raw, "Игры") || !strings.Contains(raw, "games") {
		t.Fatalf("keyboard content: %q", raw)
	}
}

func TestGetConversationTitle(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "messages.getConversationsById") {
			t.Errorf("method path: %q", r.URL.Path)
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		got = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":{"count":1,"items":[{"chat_settings":{"title":"Волейбол Иваново"}}]}}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	title, err := c.GetConversationTitle(context.Background(), 2_000_000_047)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Волейбол Иваново" {
		t.Fatalf("title: %q", title)
	}
	params, err := url.ParseQuery(got)
	if err != nil {
		t.Fatal(err)
	}
	if params.Get("peer_ids") != "2000000047" {
		t.Fatalf("peer_ids: %q", params.Get("peer_ids"))
	}
}

func TestGetConversationTitleEmptyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":{"count":0,"items":[]}}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	if _, err := c.GetConversationTitle(context.Background(), 555); err == nil {
		t.Fatal("want error when VK returns no conversation")
	}
}

func TestIsConversationAdmin(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "messages.getConversationMembers") {
			t.Errorf("method path: %q", r.URL.Path)
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		got = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":{"count":3,"items":[` +
			`{"member_id":555,"is_admin":1},` +
			`{"member_id":777,"is_admin":0},` +
			`{"member_id":888,"is_owner":1}]}}`))
	}))
	defer srv.Close()

	c := NewClient("tok", "123", WithBaseURL(srv.URL))
	ctx := context.Background()

	cases := []struct {
		name   string
		userID int64
		want   bool
	}{
		{"administrator", 555, true},
		{"owner", 888, true},
		{"plain member", 777, false},
		{"not a member", 999, false},
	}
	for _, tc := range cases {
		isAdmin, err := c.IsConversationAdmin(ctx, 2_000_000_047, tc.userID)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if isAdmin != tc.want {
			t.Fatalf("%s: want %v, got %v", tc.name, tc.want, isAdmin)
		}
	}

	params, err := url.ParseQuery(got)
	if err != nil {
		t.Fatal(err)
	}
	if params.Get("peer_id") != "2000000047" {
		t.Fatalf("peer_id: %q", params.Get("peer_id"))
	}
}
