package vk

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sportevents.local/internal/chat"
)

// These tests exercise the callback dispatcher with in-memory fakes for the
// messenger and stores, so they need neither PostgreSQL nor the VK API.

// --- fakes ---

type fakeMessenger struct {
	sent     []sentMessage
	answers  []answeredEvent
	userName string
}

type sentMessage struct {
	PeerID   int64
	Text     string
	Keyboard string
}

type answeredEvent struct {
	UserID  int64
	PeerID  int64
	EventID string
}

func (f *fakeMessenger) SendMessage(_ context.Context, peerID int64, text, keyboard string) (int64, error) {
	f.sent = append(f.sent, sentMessage{PeerID: peerID, Text: text, Keyboard: keyboard})
	return int64(len(f.sent)), nil
}

func (f *fakeMessenger) AnswerMessageEvent(_ context.Context, userID, peerID int64, eventID, eventData string) error {
	f.answers = append(f.answers, answeredEvent{UserID: userID, PeerID: peerID, EventID: eventID})
	return nil
}

func (f *fakeMessenger) GetUserName(_ context.Context, userID int64) (string, error) {
	if f.userName != "" {
		return f.userName, nil
	}
	return "", fmt.Errorf("no name")
}

type fakeChatStore struct {
	callCount int
	chat      *chat.Chat
}

func (f *fakeChatStore) FindByChannel(_ context.Context, platform, externalChatID string) (*chat.Chat, error) {
	f.callCount++
	if f.chat == nil {
		return nil, nil
	}
	return f.chat, nil
}

type fakeUserStore struct {
	callCount int
	user      chat.User
}

func (f *fakeUserStore) FindOrCreateUserByExternalID(_ context.Context, platform, externalUserID, displayName string) (chat.User, error) {
	f.callCount++
	f.user = chat.User{ID: 7, DisplayName: displayName, CreatedAt: time.Now()}
	return f.user, nil
}

// --- helpers ---

type harness struct {
	svc   *Service
	msg   *fakeMessenger
	chats *fakeChatStore
	users *fakeUserStore
}

func newHarness(t *testing.T, chatLinked bool) *harness {
	t.Helper()
	h := &harness{
		msg:   &fakeMessenger{userName: "Иван Петров"},
		chats: &fakeChatStore{},
		users: &fakeUserStore{},
	}
	if chatLinked {
		h.chats.chat = &chat.Chat{ID: 1, Title: "Волейбол"}
	}
	h.svc = NewService(Config{
		ConfirmationToken: "confirmation-code-123",
		Secret:            "sekret",
		GroupID:           "12345",
	}, h.msg, h.chats, h.users)
	return h
}

func postJSON(t *testing.T, handler http.HandlerFunc, payload string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, srv.URL, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

// --- tests ---

func TestCallbackConfirmationReturnsToken(t *testing.T) {
	h := newHarness(t, false)
	code, body := postJSON(t, h.svc.HandleCallback, `{"type":"confirmation"}`)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if body != "confirmation-code-123" {
		t.Fatalf("body: %q", body)
	}
	if len(h.msg.sent) != 0 {
		t.Fatal("confirmation must not send messages")
	}
}

func TestCallbackWrongSecretRejected(t *testing.T) {
	h := newHarness(t, true)
	payload := `{"type":"message_new","group_id":12345,"secret":"bad"` + `,"object":{}}`
	code, _ := postJSON(t, h.svc.HandleCallback, payload)
	if code != http.StatusForbidden {
		t.Fatalf("status: want 403, got %d", code)
	}
	if h.chats.callCount != 0 {
		t.Fatal("unauthorized event must not hit the store")
	}
}

func TestCallbackWrongGroupRejected(t *testing.T) {
	h := newHarness(t, true)
	payload := `{"type":"message_new","group_id":999,"secret":"sekret"` + `,"object":{}}`
	code, _ := postJSON(t, h.svc.HandleCallback, payload)
	if code != http.StatusForbidden {
		t.Fatalf("status: want 403, got %d", code)
	}
}

func TestCallbackMessageNewUnknownChatRepliesHint(t *testing.T) {
	h := newHarness(t, false) // no linked chat
	payload := `{"type":"message_new","group_id":12345,"secret":"sekret","object":{"message":{` +
		`"id":1,"date":0,"peer_id":2000000047,"from_id":555,"text":"/start","out":0}}}`
	code, body := postJSON(t, h.svc.HandleCallback, payload)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if body != "ok" {
		t.Fatalf("body must be ok, got %q", body)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 hint, got %d", len(h.msg.sent))
	}
	if h.msg.sent[0].PeerID != 2000000047 {
		t.Fatalf("peer: %d", h.msg.sent[0].PeerID)
	}
	if !strings.Contains(h.msg.sent[0].Text, "не подключена") {
		t.Fatalf("hint text: %q", h.msg.sent[0].Text)
	}
	if h.users.callCount != 0 {
		t.Fatal("unknown chat must not create an identity")
	}
}

func TestCallbackMessageNewStartWelcomes(t *testing.T) {
	h := newHarness(t, true)
	payload := `{"type":"message_new","group_id":12345,"secret":"sekret","object":{"message":{` +
		`"id":1,"date":0,"peer_id":2000000047,"from_id":555,"text":"/start","out":0}}}`
	code, _ := postJSON(t, h.svc.HandleCallback, payload)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.users.callCount != 1 {
		t.Fatalf("identity must be created once, got %d", h.users.callCount)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	if h.msg.sent[0].PeerID != 2000000047 {
		t.Fatalf("peer: %d", h.msg.sent[0].PeerID)
	}
	if !strings.Contains(h.msg.sent[0].Text, "Иван Петров") {
		t.Fatalf("welcome must mention name: %q", h.msg.sent[0].Text)
	}
	if !strings.Contains(h.msg.sent[0].Keyboard, "Игры") {
		t.Fatalf("welcome must carry keyboard: %q", h.msg.sent[0].Keyboard)
	}
}

func TestCallbackMessageNewButtonPayload(t *testing.T) {
	h := newHarness(t, true)
	// The "Игры" text button is delivered as message_new with a payload.
	payload := `{"type":"message_new","group_id":12345,"secret":"sekret","object":{"message":{` +
		`"id":2,"date":0,"peer_id":2000000047,"from_id":555,"text":"","out":0,"payload":"{\"command\":\"games\"}"}}}`
	code, _ := postJSON(t, h.svc.HandleCallback, payload)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	if !strings.Contains(h.msg.sent[0].Text, "игр") {
		t.Fatalf("reply: %q", h.msg.sent[0].Text)
	}
}

func TestCallbackMessageEventButtonPress(t *testing.T) {
	h := newHarness(t, true)
	payload := `{"type":"message_event","group_id":12345,"secret":"sekret","event_id":"e1","object":{` +
		`"user_id":555,"peer_id":2000000047,"event_id":"e1","payload":"{\"command\":\"my\"}"}}`
	code, _ := postJSON(t, h.svc.HandleCallback, payload)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	// The button press must be acknowledged so it stops loading in clients.
	if len(h.msg.answers) != 1 {
		t.Fatalf("want 1 event answer, got %d", len(h.msg.answers))
	}
	want := answeredEvent{UserID: 555, PeerID: 2000000047, EventID: "e1"}
	if h.msg.answers[0] != want {
		t.Fatalf("event answer: %+v, want %+v", h.msg.answers[0], want)
	}
	if h.users.callCount != 1 {
		t.Fatalf("identity: %d", h.users.callCount)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	if !strings.Contains(h.msg.sent[0].Text, "запис") {
		t.Fatalf("reply: %q", h.msg.sent[0].Text)
	}
}

func TestCallbackOwnOutgoingMessageIgnored(t *testing.T) {
	h := newHarness(t, true)
	payload := `{"type":"message_new","group_id":12345,"secret":"sekret","object":{"message":{` +
		`"id":3,"date":0,"peer_id":2000000047,"from_id":-12345,"text":"ok","out":1}}}`
	code, _ := postJSON(t, h.svc.HandleCallback, payload)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 0 {
		t.Fatal("own outgoing message must be ignored")
	}
	if h.users.callCount != 0 {
		t.Fatal("own outgoing message must not create an identity")
	}
}

// Duplicate ("retried") callbacks carry the same event_id and must be
// processed once: one reply, one identity lookup.

const helloEnvelope = `{"type":"message_new","group_id":12345,"secret":"sekret","event_id":"evt-1","object":{"message":{` +
	`"id":1,"date":0,"peer_id":2000000047,"from_id":555,"text":"/start","out":0}}}`

func TestCallbackRetriedEventProcessedOnce(t *testing.T) {
	h := newHarness(t, true)
	for i := 0; i < 2; i++ {
		code, body := postJSON(t, h.svc.HandleCallback, helloEnvelope)
		if code != http.StatusOK {
			t.Fatalf("attempt %d: status %d", i, code)
		}
		if body != "ok" {
			t.Fatalf("attempt %d: body %q", i, body)
		}
	}
	if h.users.callCount != 1 {
		t.Fatalf("identity must be resolved once, got %d", h.users.callCount)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 welcome, got %d", len(h.msg.sent))
	}
}

func TestCallbackDistinctEventsNotDeduped(t *testing.T) {
	h := newHarness(t, true)
	variants := []string{"evt-a", "evt-b"}
	for _, eid := range variants {
		payload := strings.Replace(helloEnvelope, "evt-1", eid, 1)
		code, _ := postJSON(t, h.svc.HandleCallback, payload)
		if code != http.StatusOK {
			t.Fatalf("event %s: status %d", eid, code)
		}
	}
	if len(h.msg.sent) != 2 {
		t.Fatalf("distinct events must both be processed, got %d replies", len(h.msg.sent))
	}
}

func TestCallbackRetriedEventWithoutEventIDNotDeduped(t *testing.T) {
	h := newHarness(t, true)
	payload := strings.Replace(helloEnvelope, `"event_id":"evt-1",`, "", 1)
	for i := 0; i < 2; i++ {
		code, _ := postJSON(t, h.svc.HandleCallback, payload)
		if code != http.StatusOK {
			t.Fatalf("attempt %d: status %d", i, code)
		}
	}
	// No event_id in the envelope → dedup cannot help; the DB unique index on
	// the identity remains the real safety net.
	if len(h.msg.sent) != 2 {
		t.Fatalf("events without event_id are not deduped: %d replies", len(h.msg.sent))
	}
}

func TestCallbackRejectedSecretNotMarkedDeduped(t *testing.T) {
	h := newHarness(t, true)
	bad := strings.Replace(helloEnvelope, `"secret":"sekret"`, `"secret":"wrong"`, 1)
	if code, _ := postJSON(t, h.svc.HandleCallback, bad); code != http.StatusForbidden {
		t.Fatalf("wrong secret: status %d, want 403", code)
	}
	// The rejected retry must not consume the event id: the legit delivery
	// with the same event_id is still processed.
	code, _ := postJSON(t, h.svc.HandleCallback, helloEnvelope)
	if code != http.StatusOK {
		t.Fatalf("legit delivery: status %d", code)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 welcome, got %d", len(h.msg.sent))
	}
}
