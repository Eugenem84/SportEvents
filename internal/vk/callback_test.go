package vk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"sportevents.local/internal/announce"
	"sportevents.local/internal/booking"
	"sportevents.local/internal/chat"
	"sportevents.local/internal/event"
	"sportevents.local/internal/schedule"
)

// These tests exercise the callback dispatcher with in-memory fakes for the
// messenger and stores, so they need neither PostgreSQL nor the VK API.

// --- fakes ---

type fakeMessenger struct {
	sent     []sentMessage
	edits    []editedMessage
	answers  []answeredEvent
	pinned   []int64
	userName string
	// editErr makes EditMessage fail: обновление анонса не должно ломать
	// действие человека, который нажал кнопку.
	editErr error
	// sendZero imitates VK: в беседе messages.send отвечает 0 вместо id, и
	// тогда id последнего своего сообщения читается из беседы (lastCMID).
	sendZero bool
	lastCMID int64
}

type sentMessage struct {
	PeerID   int64
	Text     string
	Keyboard string
}

type editedMessage struct {
	PeerID    int64
	MessageID int64
	Text      string
	Keyboard  string
}

type answeredEvent struct {
	UserID    int64
	PeerID    int64
	EventID   string
	EventData string
}

func (f *fakeMessenger) SendMessage(_ context.Context, peerID int64, text, keyboard string) (int64, error) {
	f.sent = append(f.sent, sentMessage{PeerID: peerID, Text: text, Keyboard: keyboard})
	if f.sendZero {
		return 0, nil
	}
	return int64(len(f.sent)), nil
}

func (f *fakeMessenger) LastOwnConversationMessageID(_ context.Context, peerID int64) (int64, error) {
	return f.lastCMID, nil
}

func (f *fakeMessenger) EditMessage(_ context.Context, peerID, messageID int64, text, keyboard string) (int64, error) {
	if f.editErr != nil {
		return 0, f.editErr
	}
	f.edits = append(f.edits, editedMessage{PeerID: peerID, MessageID: messageID, Text: text, Keyboard: keyboard})
	return messageID, nil
}

func (f *fakeMessenger) PinMessage(_ context.Context, peerID, messageID int64) error {
	f.pinned = append(f.pinned, messageID)
	return nil
}

func (f *fakeMessenger) AnswerMessageEvent(_ context.Context, userID, peerID int64, eventID, eventData string) error {
	f.answers = append(f.answers, answeredEvent{UserID: userID, PeerID: peerID, EventID: eventID, EventData: eventData})
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

	connectCalls   int
	connectInput   chat.ConnectInput
	connectedChat  chat.Chat
	connectCreated bool

	// isAdmin is what IsChatAdmin reports; tests flip it to check the
	// "administrator only" refusal.
	isAdmin bool
}

func (f *fakeChatStore) FindByChannel(_ context.Context, platform, externalChatID string) (*chat.Chat, error) {
	f.callCount++
	if f.chat == nil {
		return nil, nil
	}
	return f.chat, nil
}

func (f *fakeChatStore) Connect(_ context.Context, in chat.ConnectInput) (chat.Chat, bool, error) {
	f.connectCalls++
	f.connectInput = in
	if f.connectedChat.ID == 0 {
		f.connectedChat = chat.Chat{ID: 42, Title: in.ChatTitle}
	}
	return f.connectedChat, f.connectCreated, nil
}

func (f *fakeChatStore) IsChatAdmin(_ context.Context, chatID, userID int64) (bool, error) {
	return f.isAdmin, nil
}

type fakeEventStore struct {
	games   []event.EventSummary
	created []event.CreateInput
	getErr  error
}

func (f *fakeEventStore) Create(_ context.Context, in event.CreateInput) (event.Event, error) {
	f.created = append(f.created, in)
	return event.Event{
		ID:       100 + int64(len(f.created)),
		ChatID:   in.ChatID,
		StartsAt: in.StartsAt,
		Title:    in.Title,
		Location: in.Location,
		Capacity: in.Capacity,
	}, nil
}

func (f *fakeEventStore) Get(_ context.Context, chatID, eventID int64) (event.Event, error) {
	if f.getErr != nil {
		return event.Event{}, f.getErr
	}
	for _, g := range f.games {
		if g.ID == eventID {
			return g.Event, nil
		}
	}
	return event.Event{}, event.ErrNotFound
}

func (f *fakeEventStore) ListUpcomingWithCounts(_ context.Context, chatID int64, from time.Time, limit int) ([]event.EventSummary, error) {
	return f.games, nil
}

type fakeBookingStore struct {
	byEvent      map[int64][]booking.Booking
	active       []booking.BookingWithEvent
	created      []booking.CreateInput
	cancelled    []int64
	promote      *booking.Booking
	createErr    error
	createStatus booking.Status
}

func (f *fakeBookingStore) Create(_ context.Context, in booking.CreateInput) (booking.Booking, error) {
	if f.createErr != nil {
		return booking.Booking{}, f.createErr
	}
	f.created = append(f.created, in)

	status := f.createStatus
	if status == "" {
		status = booking.StatusConfirmed
	}
	b := booking.Booking{
		ID:         500 + int64(len(f.created)),
		EventID:    in.EventID,
		PlayerName: in.PlayerName,
		UserID:     in.UserID,
		Status:     status,
	}
	if status == booking.StatusConfirmed {
		seat := 1 + len(f.confirmed(in.EventID))
		b.SeatNo = &seat
	}

	// The fake keeps its own state, so ListByEvent and the reserve position
	// see what Create just handed out.
	if f.byEvent == nil {
		f.byEvent = map[int64][]booking.Booking{}
	}
	f.byEvent[in.EventID] = append(f.byEvent[in.EventID], b)
	return b, nil
}

// confirmed lists the confirmed bookings the fake already handed out.
func (f *fakeBookingStore) confirmed(eventID int64) []booking.Booking {
	var out []booking.Booking
	for _, b := range f.byEvent[eventID] {
		if b.Status == booking.StatusConfirmed {
			out = append(out, b)
		}
	}
	return out
}

func (f *fakeBookingStore) Cancel(_ context.Context, eventID, bookingID int64) (booking.CancelResult, error) {
	f.cancelled = append(f.cancelled, bookingID)
	res := booking.CancelResult{
		Cancelled: booking.Booking{ID: bookingID, EventID: eventID, Status: booking.StatusCancelled},
	}
	if f.promote != nil {
		res.Promoted = f.promote
	}
	return res, nil
}

func (f *fakeBookingStore) ListByEvent(_ context.Context, eventID int64, statuses ...booking.Status) ([]booking.Booking, error) {
	return f.byEvent[eventID], nil
}

func (f *fakeBookingStore) ListActiveByUser(_ context.Context, userID int64, from time.Time) ([]booking.BookingWithEvent, error) {
	return f.active, nil
}

type fakeScheduleStore struct {
	slots    []schedule.Slot
	setCalls []schedule.Slot
	removed  []int
}

func (f *fakeScheduleStore) List(_ context.Context, chatID int64) ([]schedule.Slot, error) {
	return f.slots, nil
}

func (f *fakeScheduleStore) Set(_ context.Context, chatID int64, weekday, minutes int) (schedule.Slot, error) {
	slot := schedule.Slot{ChatID: chatID, Weekday: weekday, Minutes: minutes}
	f.setCalls = append(f.setCalls, slot)

	for i := range f.slots {
		if f.slots[i].Weekday == weekday {
			f.slots[i] = slot
			return slot, nil
		}
	}
	f.slots = append(f.slots, slot)
	sort.Slice(f.slots, func(i, j int) bool { return f.slots[i].Weekday < f.slots[j].Weekday })
	return slot, nil
}

func (f *fakeScheduleStore) Delete(_ context.Context, chatID int64, weekday int) (bool, error) {
	f.removed = append(f.removed, weekday)
	for i := range f.slots {
		if f.slots[i].Weekday == weekday {
			f.slots = append(f.slots[:i], f.slots[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

type fakeAnnounceStore struct {
	saved []announce.Ref
	ref   announce.Ref
	has   bool
}

func (f *fakeAnnounceStore) Save(_ context.Context, ref announce.Ref) error {
	f.saved = append(f.saved, ref)
	f.ref, f.has = ref, true
	return nil
}

func (f *fakeAnnounceStore) Get(_ context.Context, eventID int64, platform string) (announce.Ref, bool, error) {
	if f.has && f.ref.EventID == eventID {
		return f.ref, true, nil
	}
	return announce.Ref{}, false, nil
}

type fakeConversations struct {
	title      string
	admin      bool
	titleErr   error
	adminErr   error
	adminCalls int
}

func (f *fakeConversations) GetConversationTitle(_ context.Context, peerID int64) (string, error) {
	if f.titleErr != nil {
		return "", f.titleErr
	}
	return f.title, nil
}

func (f *fakeConversations) IsConversationAdmin(_ context.Context, peerID, userID int64) (bool, error) {
	f.adminCalls++
	if f.adminErr != nil {
		return false, f.adminErr
	}
	return f.admin, nil
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
	svc    *Service
	msg    *fakeMessenger
	chats  *fakeChatStore
	users  *fakeUserStore
	convs  *fakeConversations
	events *fakeEventStore
	books  *fakeBookingStore
	anns   *fakeAnnounceStore
	sched  *fakeScheduleStore
}

// harnessNow is the pinned clock: tests assert on "сегодня/завтра 19:00".
func harnessNow() time.Time {
	return time.Date(2026, 9, 25, 12, 0, 0, 0, defaultLocation)
}

func newHarness(t *testing.T, chatLinked bool) *harness {
	t.Helper()
	h := &harness{
		msg:    &fakeMessenger{userName: "Иван Петров"},
		chats:  &fakeChatStore{connectCreated: true, isAdmin: true},
		users:  &fakeUserStore{},
		convs:  &fakeConversations{title: "Волейбол Иваново", admin: true},
		events: &fakeEventStore{},
		books:  &fakeBookingStore{},
		anns:   &fakeAnnounceStore{},
		sched:  &fakeScheduleStore{},
	}
	if chatLinked {
		h.chats.chat = &chat.Chat{ID: 1, Title: "Волейбол"}
	}
	h.svc = NewService(Config{
		ConfirmationToken: "confirmation-code-123",
		Secret:            "sekret",
		GroupID:           "12345",
	}, Deps{
		Messenger: h.msg,
		Chats:     h.chats,
		Users:     h.users,
		Convs:     h.convs,
		Events:    h.events,
		Bookings:  h.books,
		Announces: h.anns,
		Schedule:  h.sched,
		Now:       harnessNow,
	})
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

// В неподключённой беседе бот тоже не вмешивается в разговор: он реагирует
// только на запрос подключения.
func TestCallbackUnconnectedChatIgnoresSmallTalk(t *testing.T) {
	h := newHarness(t, false) // no linked chat
	code, body := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/start", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if body != "ok" {
		t.Fatalf("body must be ok, got %q", body)
	}
	if len(h.msg.sent) != 0 {
		t.Fatalf("an unconnected chat must stay silent, got %+v", h.msg.sent)
	}
	if h.users.callCount != 0 {
		t.Fatal("unknown chat must not create an identity")
	}
}

func TestCallbackMessageNewStartWelcomes(t *testing.T) {
	h := newHarness(t, true)
	payload := `{"type":"message_new","group_id":12345,"secret":"sekret","object":{"message":{` +
		`"id":1,"date":0,"peer_id":2000000047,"from_id":555,"text":"/помощь","out":0}}}`
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
	// Игр нет — кнопки записи не показываем: клавиатура пустая.
	if strings.Contains(h.msg.sent[0].Keyboard, "Иду") {
		t.Fatalf("no game is open, so the reply must hide the sign-up buttons: %q", h.msg.sent[0].Keyboard)
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
	`"id":1,"date":0,"peer_id":2000000047,"from_id":555,"text":"/помощь","out":0}}}`

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

// --- connect (Phase 5) ---

// connectEnvelope is a message_new asking to connect the conversation.
func connectEnvelope(peerID, fromID int64, text string) string {
	return fmt.Sprintf(`{"type":"message_new","group_id":12345,"secret":"sekret","object":{"message":{`+
		`"id":9,"date":0,"peer_id":%d,"from_id":%d,"text":%q,"out":0}}}`, peerID, fromID, text)
}

func TestCallbackConnectCreatesChatAndAdmin(t *testing.T) {
	h := newHarness(t, false) // conversation not connected yet
	code, _ := postJSON(t, h.svc.HandleCallback, connectEnvelope(2000000047, 555, "подключить"))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.convs.adminCalls != 1 {
		t.Fatalf("initiator rights must be checked once, got %d", h.convs.adminCalls)
	}
	if h.chats.connectCalls != 1 {
		t.Fatalf("want 1 connect, got %d", h.chats.connectCalls)
	}
	got := h.chats.connectInput
	if got.Platform != chat.PlatformVK || got.ExternalChatID != "2000000047" {
		t.Fatalf("connect input channel: %+v", got)
	}
	if got.ChatTitle != "Волейбол Иваново" {
		t.Fatalf("connect input title: %q", got.ChatTitle)
	}
	if got.InitiatorID != h.users.user.ID {
		t.Fatalf("initiator: want %d, got %d", h.users.user.ID, got.InitiatorID)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	if !strings.Contains(h.msg.sent[0].Text, "подключена") {
		t.Fatalf("reply: %q", h.msg.sent[0].Text)
	}
	if !strings.Contains(h.msg.sent[0].Text, "Волейбол Иваново") {
		t.Fatalf("reply must mention the chat title: %q", h.msg.sent[0].Text)
	}
	// Свежая беседа: игр ещё нет, поэтому кнопок нет, а текст объясняет, когда
	// они появятся.
	if strings.Contains(h.msg.sent[0].Keyboard, "Иду") {
		t.Fatalf("a fresh chat has no game, so no buttons: %q", h.msg.sent[0].Keyboard)
	}
	if !strings.Contains(h.msg.sent[0].Text, "Иду") {
		t.Fatalf("connect must tell when the buttons appear: %q", h.msg.sent[0].Text)
	}
}

func TestCallbackConnectDeniedForNonAdmin(t *testing.T) {
	h := newHarness(t, false)
	h.convs.admin = false
	code, _ := postJSON(t, h.svc.HandleCallback, connectEnvelope(2000000047, 555, "подключить"))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.chats.connectCalls != 0 {
		t.Fatal("a non-admin must not create a chat")
	}
	if len(h.msg.sent) != 1 || !strings.Contains(h.msg.sent[0].Text, "администратор") {
		t.Fatalf("reply: %+v", h.msg.sent)
	}
}

func TestCallbackConnectRejectsDirectDialog(t *testing.T) {
	h := newHarness(t, false)
	code, _ := postJSON(t, h.svc.HandleCallback, connectEnvelope(555, 555, "подключить"))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.users.callCount != 0 || h.convs.adminCalls != 0 || h.chats.connectCalls != 0 {
		t.Fatal("a one-to-one dialog must not reach identity, rights or the store")
	}
	if len(h.msg.sent) != 1 || !strings.Contains(h.msg.sent[0].Text, "бесед") {
		t.Fatalf("reply: %+v", h.msg.sent)
	}
}

// Повторное подключение — теперь явная команда со слэшем; без слэша в живой
// беседе «подключить» это просто слово в переписке.
func TestCallbackConnectAlreadyConnected(t *testing.T) {
	h := newHarness(t, true) // chat is linked already
	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/connect", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.chats.connectCalls != 0 || h.convs.adminCalls != 0 {
		t.Fatal("an already connected chat must not be connected again")
	}
	if len(h.msg.sent) != 1 || !strings.Contains(h.msg.sent[0].Text, "уже подключена") {
		t.Fatalf("reply: %+v", h.msg.sent)
	}
}

// Обычная переписка в подключённой беседе не должна вызывать бота: ни ответа,
// ни создания identity, ни обращения к VK за именем.
func TestCallbackSmallTalkIsIgnored(t *testing.T) {
	h := newHarness(t, true)

	code, body := postJSON(t, h.svc.HandleCallback,
		msgEnvelope(2000000047, 555, "ребята, кто в воскресенье играет?", ""))
	if code != http.StatusOK || body != "ok" {
		t.Fatalf("status %d body %q", code, body)
	}
	if len(h.msg.sent) != 0 {
		t.Fatalf("the bot must not answer small talk: %+v", h.msg.sent)
	}
	if h.users.callCount != 0 {
		t.Fatal("small talk must not create an identity")
	}
}

// «подключить» без слэша в подключённой беседе — уже не команда.
func TestCallbackPlainConnectInConnectedChatIsIgnored(t *testing.T) {
	h := newHarness(t, true)

	code, _ := postJSON(t, h.svc.HandleCallback, connectEnvelope(2000000047, 555, "подключить"))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 0 || h.chats.connectCalls != 0 {
		t.Fatalf("a connected chat must stay silent: sent=%+v connects=%d", h.msg.sent, h.chats.connectCalls)
	}
}

// A concurrent connect (a retried callback without event_id, or another bot
// instance) is resolved by the store: created=false must be reported as
// "already connected" instead of a second success message.
func TestCallbackConnectRaceAlreadyConnected(t *testing.T) {
	h := newHarness(t, false)
	h.chats.connectCreated = false
	h.chats.connectedChat = chat.Chat{ID: 7, Title: "Волейбол Иваново"}
	code, _ := postJSON(t, h.svc.HandleCallback, connectEnvelope(2000000047, 555, "подключить"))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 || !strings.Contains(h.msg.sent[0].Text, "уже подключена") {
		t.Fatalf("reply: %+v", h.msg.sent)
	}
}

// A missing title must not block the connection: a placeholder is stored.
func TestCallbackConnectTitleFallback(t *testing.T) {
	h := newHarness(t, false)
	h.convs.titleErr = fmt.Errorf("vk: no access")
	code, _ := postJSON(t, h.svc.HandleCallback, connectEnvelope(2000000047, 555, "подключить"))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.chats.connectCalls != 1 {
		t.Fatalf("connect must proceed without a title, got %d calls", h.chats.connectCalls)
	}
	if h.chats.connectInput.ChatTitle != "Беседа 2000000047" {
		t.Fatalf("fallback title: %q", h.chats.connectInput.ChatTitle)
	}
}

// A retried connect callback (same event_id) must not connect twice.
func TestCallbackConnectRetriedEventProcessedOnce(t *testing.T) {
	h := newHarness(t, false)
	env := strings.Replace(connectEnvelope(2000000047, 555, "подключить"),
		`"type":"message_new"`, `"type":"message_new","event_id":"conn-1"`, 1)
	for i := 0; i < 2; i++ {
		if code, _ := postJSON(t, h.svc.HandleCallback, env); code != http.StatusOK {
			t.Fatalf("attempt %d: status %d", i, code)
		}
	}
	if h.chats.connectCalls != 1 {
		t.Fatalf("retried event must connect once, got %d", h.chats.connectCalls)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
}

// A VK API failure while checking the initiator's rights must be answered,
// not swallowed: the user has to learn that the bot is not a chat admin
// (otherwise the bot just stays silent in the conversation).
func TestCallbackConnectVKAPIErrorReplies(t *testing.T) {
	h := newHarness(t, false)
	h.convs.adminErr = fmt.Errorf("vk messages.getConversationMembers: error 15: Access denied")
	code, _ := postJSON(t, h.svc.HandleCallback, connectEnvelope(2000000047, 555, "подключить"))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.chats.connectCalls != 0 {
		t.Fatal("a failed rights check must not create a chat")
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	if !strings.Contains(h.msg.sent[0].Text, "Не удалось") {
		t.Fatalf("reply: %q", h.msg.sent[0].Text)
	}
}

// --- Phase 6: игры, запись, резерв, отмена ---

// msgEnvelope is a message_new; payload, when set, is what a pressed text
// button attaches to the message.
func msgEnvelope(peerID, fromID int64, text, payload string) string {
	extra := ""
	if payload != "" {
		extra = fmt.Sprintf(`,"payload":%q`, payload)
	}
	return fmt.Sprintf(`{"type":"message_new","group_id":12345,"secret":"sekret","object":{"message":{`+
		`"id":9,"date":0,"peer_id":%d,"from_id":%d,"text":%q,"out":0%s}}}`, peerID, fromID, text, extra)
}

// firstButton decodes a keyboard JSON and returns the first button: the
// payload is a nested JSON string, so substring checks on the raw keyboard
// would only see escaped quotes.
func firstButton(t *testing.T, raw string) Button {
	t.Helper()
	var kb Keyboard
	if err := json.Unmarshal([]byte(raw), &kb); err != nil {
		t.Fatalf("keyboard json: %v (%q)", err, raw)
	}
	if len(kb.Buttons) == 0 || len(kb.Buttons[0]) == 0 {
		t.Fatalf("keyboard has no buttons: %q", raw)
	}
	return kb.Buttons[0][0]
}

func TestCallbackGamesListsFreeSlotsAndButtons(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event:     event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12},
		Confirmed: 11,
		Waitlist:  2,
		Free:      1,
	}}

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "", EventCommandPayload(cmdGames, 0)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	got := h.msg.sent[0]
	if !strings.Contains(got.Text, "сб, 26.09, 12:00") {
		t.Fatalf("time not rendered in the chat zone: %q", got.Text)
	}
	if !strings.Contains(got.Text, "свободно 1 из 12") || !strings.Contains(got.Text, "в резерве 2") {
		t.Fatalf("counters: %q", got.Text)
	}
	btn := firstButton(t, got.Keyboard)
	if cmd, eventID := ParsePayload(btn.Action.Payload); cmd != cmdAttend || eventID != 7 {
		t.Fatalf("button: %s/%d", cmd, eventID)
	}
}

func TestCallbackCreateGameByAdminPostsAnnouncement(t *testing.T) {
	h := newHarness(t, true)
	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/create 27.09 19:00 8", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.events.created) != 1 {
		t.Fatalf("created: %+v", h.events.created)
	}
	in := h.events.created[0]
	want := time.Date(2026, 9, 27, 19, 0, 0, 0, defaultLocation).UTC()
	if !in.StartsAt.Equal(want) {
		t.Fatalf("starts_at: got %v want %v", in.StartsAt, want)
	}
	if in.Capacity != 8 || in.Title != defaultTitle {
		t.Fatalf("input: %+v", in)
	}
	// Два сообщения: сам анонс со составом и строчка «Запись открыта», которая
	// приносит кнопки «Иду» / «Не иду» под поле ввода.
	if len(h.msg.sent) != 2 {
		t.Fatalf("want the announcement and the notice, got %+v", h.msg.sent)
	}
	if !strings.Contains(h.msg.sent[0].Text, "Состав (0/8)") {
		t.Fatalf("announcement: %+v", h.msg.sent[0])
	}
	if !strings.Contains(h.msg.sent[1].Text, "Запись открыта") {
		t.Fatalf("sign-up notice: %+v", h.msg.sent[1])
	}
	if !strings.Contains(h.msg.sent[1].Keyboard, "Иду") {
		t.Fatalf("the notice must bring the buttons: %q", h.msg.sent[1].Keyboard)
	}
	if len(h.anns.saved) != 1 || h.anns.saved[0].MessageID == 0 {
		t.Fatalf("announcement reference: %+v", h.anns.saved)
	}
	if h.anns.saved[0].ExternalChatID != "2000000047" || h.anns.saved[0].Platform != chat.PlatformVK {
		t.Fatalf("reference target: %+v", h.anns.saved[0])
	}
}

func TestCallbackCreateGameDeniedForNonAdmin(t *testing.T) {
	h := newHarness(t, true)
	h.chats.isAdmin = false

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/create", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.events.created) != 0 {
		t.Fatal("a non-admin must not create games")
	}
	if len(h.msg.sent) != 1 || !strings.Contains(h.msg.sent[0].Text, "администратор") {
		t.Fatalf("reply: %+v", h.msg.sent)
	}
}

// snackbar returns the personal message the presser saw: VK's event_data of
// the last button answer.
func snackbar(t *testing.T, h *harness) string {
	t.Helper()
	if len(h.msg.answers) == 0 {
		t.Fatal("the button press was not answered")
	}
	return h.msg.answers[len(h.msg.answers)-1].EventData
}

// Нажатие «Иду»: в чат уходит строчка «номер - имя», лично — всплывашка, а
// анонс переписывается на месте.
func TestCallbackAttendPostsSeatLineAndSnackbar(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12},
	}}
	h.anns.ref = announce.Ref{EventID: 7, Platform: chat.PlatformVK, ExternalChatID: "2000000047", MessageID: 555}
	h.anns.has = true

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 900, EventCommandPayload(cmdAttend, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.books.created) != 1 || h.books.created[0].EventID != 7 {
		t.Fatalf("created: %+v", h.books.created)
	}
	if u := h.books.created[0].UserID; u == nil || *u != h.users.user.ID {
		t.Fatalf("booking must carry the identity: %+v", h.books.created[0])
	}
	if len(h.msg.sent) != 1 || h.msg.sent[0].Text != "1 - Иван Петров" {
		t.Fatalf("seat line: %+v", h.msg.sent)
	}
	if !strings.Contains(snackbar(t, h), "Вы записаны: место 1") {
		t.Fatalf("snackbar: %q", snackbar(t, h))
	}
	if len(h.msg.edits) != 1 || h.msg.edits[0].MessageID != 555 {
		t.Fatalf("announcement must be rewritten in place: %+v", h.msg.edits)
	}
}

// Полная игра: «Иду» отправляет в резерв и говорит об этом лично.
func TestCallbackAttendFullGameGoesToReserve(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event:     event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 1},
		Confirmed: 1,
		Free:      0,
	}}
	h.books.createStatus = booking.StatusWaitlist

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 901, EventCommandPayload(cmdAttend, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 || h.msg.sent[0].Text != "резерв 1 - Иван Петров" {
		t.Fatalf("reserve line: %+v", h.msg.sent)
	}
	if !strings.Contains(snackbar(t, h), "в резерве") {
		t.Fatalf("snackbar: %q", snackbar(t, h))
	}
}

// Анонс: дата с днём недели, номера мест с «свободно» вместо уехавших номеров,
// счётчик свободных мест и резерв в порядке очереди.
func TestCallbackAnnouncementShowsSeatsAndReserve(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 3},
	}}
	h.anns.ref = announce.Ref{EventID: 7, Platform: chat.PlatformVK, ExternalChatID: "2000000047", MessageID: 555}
	h.anns.has = true
	seat1, seat3 := 1, 3
	h.books.byEvent = map[int64][]booking.Booking{7: {
		{ID: 1, EventID: 7, PlayerName: "Иван", SeatNo: &seat1, Status: booking.StatusConfirmed},
		{ID: 3, EventID: 7, PlayerName: "Сергей", SeatNo: &seat3, Status: booking.StatusConfirmed},
		{ID: 4, EventID: 7, PlayerName: "Мария", Status: booking.StatusWaitlist},
	}}
	h.books.createStatus = booking.StatusWaitlist

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 902, EventCommandPayload(cmdAttend, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.edits) != 1 {
		t.Fatalf("announcement must be rewritten: %+v", h.msg.edits)
	}
	edit := h.msg.edits[0]
	for _, want := range []string{
		"суббота, 26 сентября, 12:00",
		"Состав (2/3)",
		"1. Иван · 2. свободно · 3. Сергей",
		"Свободно мест: 1",
		"Резерв (2): Мария · Иван Петров",
	} {
		if !strings.Contains(edit.Text, want) {
			t.Fatalf("announcement must contain %q:\n%s", want, edit.Text)
		}
	}
}

// «Не иду» пишет в чат «номер - имя минус» и сообщает, кто поднялся из резерва.
func TestCallbackSkipPostsMinusLineAndPromotion(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12},
	}}
	seat := 3
	h.books.active = []booking.BookingWithEvent{{
		Booking: booking.Booking{
			ID: 55, EventID: 7, PlayerName: "Иван Петров", SeatNo: &seat, Status: booking.StatusConfirmed,
		},
		EventTitle:    "Волейбол",
		EventStartsAt: harnessNow().Add(24 * time.Hour),
	}}
	promotedSeat := 3
	promoted := booking.Booking{
		ID: 56, EventID: 7, PlayerName: "Пётр", SeatNo: &promotedSeat, Status: booking.StatusConfirmed,
	}
	h.books.promote = &promoted
	h.anns.ref = announce.Ref{EventID: 7, Platform: chat.PlatformVK, ExternalChatID: "2000000047", MessageID: 555}
	h.anns.has = true

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 903, EventCommandPayload(cmdSkip, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.books.cancelled) != 1 || h.books.cancelled[0] != 55 {
		t.Fatalf("cancelled: %+v", h.books.cancelled)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 chat line, got %d", len(h.msg.sent))
	}
	for _, want := range []string{"3 - Иван Петров минус", "3 - Пётр из резерва"} {
		if !strings.Contains(h.msg.sent[0].Text, want) {
			t.Fatalf("chat line must contain %q: %q", want, h.msg.sent[0].Text)
		}
	}
	if !strings.Contains(snackbar(t, h), "Запись отменена") {
		t.Fatalf("snackbar: %q", snackbar(t, h))
	}
	if len(h.msg.edits) != 1 {
		t.Fatalf("announcement must be rewritten: %+v", h.msg.edits)
	}
}

// «Не иду», когда человек не записан: в чат не пишем ничего, отвечаем лично.
func TestCallbackSkipWithoutBookingStaysSilent(t *testing.T) {
	h := newHarness(t, true)

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 904, EventCommandPayload(cmdSkip, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.books.cancelled) != 0 {
		t.Fatalf("nothing to cancel: %+v", h.books.cancelled)
	}
	if len(h.msg.sent) != 0 {
		t.Fatalf("the chat must stay silent: %+v", h.msg.sent)
	}
	if !strings.Contains(snackbar(t, h), "не записаны") {
		t.Fatalf("snackbar: %q", snackbar(t, h))
	}
}

func TestCallbackMyBookings(t *testing.T) {
	h := newHarness(t, true)
	h.books.active = []booking.BookingWithEvent{{
		Booking:       booking.Booking{ID: 55, EventID: 7, Status: booking.StatusWaitlist},
		EventTitle:    "Волейбол",
		EventStartsAt: harnessNow().Add(24 * time.Hour),
	}}

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/my", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	got := h.msg.sent[0]
	if !strings.Contains(got.Text, "в резерве") || !strings.Contains(got.Text, "сб, 26.09, 12:00") {
		t.Fatalf("text: %q", got.Text)
	}
	btn := firstButton(t, got.Keyboard)
	if cmd, eventID := ParsePayload(btn.Action.Payload); cmd != cmdCancelBooking || eventID != 7 {
		t.Fatalf("button: %s/%d", cmd, eventID)
	}
}

func TestParseCreateGame(t *testing.T) {
	now := harnessNow()
	cases := []struct {
		name      string
		text      string
		wantStart time.Time
		wantCap   int
		wantTitle string
	}{
		{
			name:      "defaults",
			text:      "создать игру",
			wantStart: time.Date(2026, 9, 26, 19, 0, 0, 0, defaultLocation).UTC(),
			wantCap:   defaultCapacity,
			wantTitle: defaultTitle,
		},
		{
			name:      "date, time and capacity",
			text:      "создать игру 27.09 20:30 8",
			wantStart: time.Date(2026, 9, 27, 20, 30, 0, 0, defaultLocation).UTC(),
			wantCap:   8,
			wantTitle: defaultTitle,
		},
		{
			name:      "title kept",
			text:      "создать игру 28.09 19:00 пляжный волейбол",
			wantStart: time.Date(2026, 9, 28, 19, 0, 0, 0, defaultLocation).UTC(),
			wantCap:   defaultCapacity,
			wantTitle: "пляжный волейбол",
		},
	}

	for _, tc := range cases {
		got := parseCreateGame(tc.text, now, defaultLocation)
		if !got.startsAt.Equal(tc.wantStart) {
			t.Fatalf("%s: starts_at got %v want %v", tc.name, got.startsAt, tc.wantStart)
		}
		if got.capacity != tc.wantCap {
			t.Fatalf("%s: capacity got %d want %d", tc.name, got.capacity, tc.wantCap)
		}
		if got.title != tc.wantTitle {
			t.Fatalf("%s: title got %q want %q", tc.name, got.title, tc.wantTitle)
		}
	}
}

func TestCallbackSlotAddHintWithoutParameters(t *testing.T) {
	h := newHarness(t, true)

	payload := CommandPayload(cmdSlotAdd)
	code, _ := postJSON(t, h.svc.HandleCallback, eventEnvelope(2000000047, 555, 777, payload))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 || !strings.Contains(h.msg.sent[0].Text, "вс 10:00") {
		t.Fatalf("hint: %+v", h.msg.sent)
	}
	if len(h.sched.setCalls) != 0 {
		t.Fatal("the hint button must not change the schedule")
	}
}

// С расписанием «создать игру» не требует даты: берётся ближайший слот.
func TestCallbackCreateGameUsesSchedule(t *testing.T) {
	h := newHarness(t, true)
	h.sched.slots = []schedule.Slot{{ChatID: 1, Weekday: 0, Minutes: 10 * 60}}

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/create", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.events.created) != 1 {
		t.Fatalf("created: %+v", h.events.created)
	}
	// Окно теста — пятница 25.09.2026, ближайшее воскресенье 27.09.
	want := time.Date(2026, 9, 27, 10, 0, 0, 0, defaultLocation).UTC()
	if got := h.events.created[0].StartsAt; !got.Equal(want) {
		t.Fatalf("starts_at: %v want %v", got.In(defaultLocation), want.In(defaultLocation))
	}
}

// Админ может назвать день недели словами — расписание при этом не нужно.
func TestCallbackCreateGameByWeekdayWord(t *testing.T) {
	h := newHarness(t, true)

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/create в среду 19:00", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.events.created) != 1 {
		t.Fatalf("created: %+v", h.events.created)
	}
	want := time.Date(2026, 9, 30, 19, 0, 0, 0, defaultLocation).UTC()
	in := h.events.created[0]
	if !in.StartsAt.Equal(want) {
		t.Fatalf("starts_at: %v want %v", in.StartsAt.In(defaultLocation), want.In(defaultLocation))
	}
	if in.Title != defaultTitle {
		t.Fatalf("the weekday word must not leak into the title: %q", in.Title)
	}
}

// Одно нажатие «Создать игру» — один анонс: повтор не должен создать вторую
// игру на то же время.
func TestCallbackCreateGameSkipsExisting(t *testing.T) {
	h := newHarness(t, true)
	h.sched.slots = []schedule.Slot{{ChatID: 1, Weekday: 0, Minutes: 10 * 60}}
	h.events.games = []event.EventSummary{{
		Event: event.Event{
			ID:       7,
			Title:    "Волейбол",
			StartsAt: time.Date(2026, 9, 27, 10, 0, 0, 0, defaultLocation),
		},
	}}

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/create", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.events.created) != 0 {
		t.Fatal("the same game must not be announced twice")
	}
	if len(h.msg.sent) != 2 {
		t.Fatalf("want the announcement and the note, got %+v", h.msg.sent)
	}
	if !strings.Contains(h.msg.sent[0].Text, "Состав") {
		t.Fatalf("the announcement must be posted again: %+v", h.msg.sent[0])
	}
	if !strings.Contains(h.msg.sent[1].Text, "уже создана") {
		t.Fatalf("note: %+v", h.msg.sent[1])
	}
}

// Кнопка старого анонса не записывает на прошедшую игру: запись на неё закрыта.
func TestCallbackAttendPastGameIsRefused(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(-2 * time.Hour), Capacity: 12},
	}}

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 905, AnnouncementCommandPayload(cmdAttend, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.books.created) != 0 {
		t.Fatalf("a past game must not take bookings: %+v", h.books.created)
	}
	if len(h.msg.sent) != 0 {
		t.Fatalf("the chat must stay silent: %+v", h.msg.sent)
	}
	if !strings.Contains(snackbar(t, h), "Запись закрыта") {
		t.Fatalf("snackbar: %q", snackbar(t, h))
	}
}

// --- Phase 6: id сообщения анонса ---

// Нажатие кнопки самого анонса доносит id этого сообщения: в беседе
// messages.send отвечает 0, поэтому только так бот узнаёт, что переписывать.
func TestCallbackAnnouncementPressLearnsMessageID(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12},
	}}
	// Анонс отправлен, но VK вернул 0: id пока неизвестен.
	h.anns.ref = announce.Ref{EventID: 7, Platform: chat.PlatformVK, ExternalChatID: "2000000047"}
	h.anns.has = true

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 777, AnnouncementCommandPayload(cmdAttend, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.anns.ref.MessageID != 777 {
		t.Fatalf("the announcement id must be remembered: %+v", h.anns.saved)
	}
	if len(h.msg.pinned) != 1 || h.msg.pinned[0] != 777 {
		t.Fatalf("the announcement must be pinned once its id is known: %+v", h.msg.pinned)
	}
	if len(h.msg.sent) != 1 || h.msg.sent[0].Text != "1 - Иван Петров" {
		t.Fatalf("seat line: %+v", h.msg.sent)
	}
	if len(h.msg.edits) != 1 || h.msg.edits[0].MessageID != 777 {
		t.Fatalf("announcement must be rewritten with the learned id: %+v", h.msg.edits)
	}
}

// Известный id не подменяется нажатием на другое сообщение: анонс всегда один
// и тот же, иначе состав начал бы обновлять чужую переписку.
func TestCallbackAnnouncementIDNotOverwritten(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12},
	}}
	h.anns.ref = announce.Ref{EventID: 7, Platform: chat.PlatformVK, ExternalChatID: "2000000047", MessageID: 615}
	h.anns.has = true

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 777, AnnouncementCommandPayload(cmdAttend, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.anns.saved) != 0 {
		t.Fatalf("the known id must stay: %+v", h.anns.saved)
	}
	if len(h.msg.edits) != 1 || h.msg.edits[0].MessageID != 615 {
		t.Fatalf("announcement must keep its message: %+v", h.msg.edits)
	}
}

// Анонс живёт в своей беседе: id, пришедший из чужой, не подходит.
func TestCallbackAnnouncementIDFromAnotherChatIgnored(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12},
	}}
	h.anns.ref = announce.Ref{EventID: 7, Platform: chat.PlatformVK, ExternalChatID: "2000000999"}
	h.anns.has = true

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 777, AnnouncementCommandPayload(cmdAttend, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.anns.ref.MessageID != 0 {
		t.Fatalf("another chat must not hand the id over: %+v", h.anns.saved)
	}
}

// Кнопка из списка игр не считается анонсом: её нажатие не должно связывать id
// этого сообщения с анонсом.
func TestCallbackGamesListPressDoesNotLearnAnnouncementID(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12},
	}}
	h.anns.ref = announce.Ref{EventID: 7, Platform: chat.PlatformVK, ExternalChatID: "2000000047"}
	h.anns.has = true

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 777, EventCommandPayload(cmdAttend, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if h.anns.ref.MessageID != 0 {
		t.Fatalf("a list button must not claim the announcement id: %+v", h.anns.saved)
	}
}

// Неудача с обновлением анонса не отменяет запись: человек уже записан, и ему
// отвечают обычной строчкой и всплывашкой.
func TestCallbackAnnouncementEditFailureKeepsBooking(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12},
	}}
	h.anns.ref = announce.Ref{EventID: 7, Platform: chat.PlatformVK, ExternalChatID: "2000000047", MessageID: 615}
	h.anns.has = true
	h.msg.editErr = errors.New("vk: 15 Access denied")

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 900, EventCommandPayload(cmdAttend, 7)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.books.created) != 1 {
		t.Fatalf("the booking must happen: %+v", h.books.created)
	}
	if len(h.msg.sent) != 1 || h.msg.sent[0].Text != "1 - Иван Петров" {
		t.Fatalf("seat line: %+v", h.msg.sent)
	}
	if !strings.Contains(snackbar(t, h), "Вы записаны: место 1") {
		t.Fatalf("snackbar: %q", snackbar(t, h))
	}
}

// В беседе VK отвечает на messages.send нулём: id анонса бот читает из самой
// беседы (последнее сообщение — только что отправленный анонс) и запоминает, а
// заодно пробует закрепить его.
func TestCallbackCreateGameLearnsAnnouncementIDFromChat(t *testing.T) {
	h := newHarness(t, true)
	h.msg.sendZero = true
	h.msg.lastCMID = 615

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/старт", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 2 || !strings.Contains(h.msg.sent[0].Text, "Состав") {
		t.Fatalf("announcement: %+v", h.msg.sent)
	}
	if h.anns.ref.MessageID != 615 {
		t.Fatalf("the announcement id must be read from the chat: %+v", h.anns.saved)
	}
	if len(h.msg.pinned) != 1 || h.msg.pinned[0] != 615 {
		t.Fatalf("the announcement must be pinned: %+v", h.msg.pinned)
	}
}

// Если id прочитать не удалось, анонс всё равно отправлен: id выучится по
// нажатию на кнопку самого анонса.
func TestCallbackCreateGameUnknownAnnouncementID(t *testing.T) {
	h := newHarness(t, true)
	h.msg.sendZero = true // lastCMID остаётся нулевым

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/старт", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 2 {
		t.Fatalf("announcement must be posted: %+v", h.msg.sent)
	}
	if h.anns.ref.MessageID != 0 {
		t.Fatalf("the id stays unknown: %+v", h.anns.saved)
	}
	if len(h.msg.pinned) != 0 {
		t.Fatalf("there is nothing to pin: %+v", h.msg.pinned)
	}
}

// --- Phase 6: расписание (настройки) ---

// eventEnvelope is a message_event — a callback button press. cmid is the id
// of the message the button was pressed on: that is how the settings screen
// knows which message to rewrite.
func eventEnvelope(peerID, userID, cmid int64, payload string) string {
	return fmt.Sprintf(`{"type":"message_event","group_id":12345,"secret":"sekret","event_id":"e1","object":{`+
		`"user_id":%d,"peer_id":%d,"event_id":"e1","conversation_message_id":%d,"payload":%q}}`,
		userID, peerID, cmid, payload)
}

func TestCallbackSettingsScreenForAdmin(t *testing.T) {
	h := newHarness(t, true)
	h.sched.slots = []schedule.Slot{{ChatID: 1, Weekday: 0, Minutes: 10 * 60}}

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/settings", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	got := h.msg.sent[0]
	if !strings.Contains(got.Text, "Сейчас: вс 10:00") {
		t.Fatalf("current schedule: %q", got.Text)
	}
	if !strings.Contains(got.Text, defaultTitle) {
		t.Fatalf("game name: %q", got.Text)
	}
	btn := firstButton(t, got.Keyboard)
	if btn.Action.Type != "callback" {
		t.Fatalf("settings buttons must be callback buttons, got %q", btn.Action.Type)
	}
	if cmd, _ := ParsePayload(btn.Action.Payload); cmd != cmdSlotRemove {
		t.Fatalf("first button: %q", btn.Action.Payload)
	}
}

// Экран настроек, открытый кнопкой, переписывает то самое сообщение: иначе в
// беседе копился бы новый экран на каждое нажатие.
func TestCallbackSettingsButtonRewritesItsMessage(t *testing.T) {
	h := newHarness(t, true)
	h.sched.slots = []schedule.Slot{{ChatID: 1, Weekday: 0, Minutes: 10 * 60}}

	code, _ := postJSON(t, h.svc.HandleCallback,
		eventEnvelope(2000000047, 555, 85, CommandPayload(cmdSettings)))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 0 {
		t.Fatalf("a press must not post a new screen: %+v", h.msg.sent)
	}
	if len(h.msg.edits) != 1 || h.msg.edits[0].MessageID != 85 {
		t.Fatalf("edits: %+v", h.msg.edits)
	}
	if !strings.Contains(h.msg.edits[0].Text, "Сейчас: вс 10:00") {
		t.Fatalf("screen: %q", h.msg.edits[0].Text)
	}
	if strings.HasPrefix(h.msg.edits[0].Text, "\n") {
		t.Fatalf("the screen must not start with an empty note line: %q", h.msg.edits[0].Text)
	}
}

func TestCallbackSettingsDeniedForNonAdmin(t *testing.T) {
	h := newHarness(t, true)
	h.chats.isAdmin = false

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/settings", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 || !strings.Contains(h.msg.sent[0].Text, "администратор") {
		t.Fatalf("reply: %+v", h.msg.sent)
	}
	if len(h.sched.setCalls) != 0 {
		t.Fatal("a non-admin must not change the schedule")
	}
}

func TestCallbackSlotAddByText(t *testing.T) {
	h := newHarness(t, true)

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/slot добавить в воскресенье 10:00", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.sched.setCalls) != 1 {
		t.Fatalf("set: %+v", h.sched.setCalls)
	}
	if slot := h.sched.setCalls[0]; slot.Weekday != 0 || slot.Minutes != 600 {
		t.Fatalf("slot: %+v", slot)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want the settings screen, got %d", len(h.msg.sent))
	}
	if !strings.Contains(h.msg.sent[0].Text, "Записано: воскресенье в 10:00") {
		t.Fatalf("note: %q", h.msg.sent[0].Text)
	}
	if !strings.Contains(h.msg.sent[0].Text, "Сейчас: вс 10:00") {
		t.Fatalf("screen after change: %q", h.msg.sent[0].Text)
	}
}

// A callback press carries the id of its message, so removing a weekday
// rewrites the settings screen in place instead of posting a new one.
func TestCallbackSlotRemoveEditsSettingsMessage(t *testing.T) {
	h := newHarness(t, true)
	h.sched.slots = []schedule.Slot{{ChatID: 1, Weekday: 3, Minutes: 19 * 60}}

	payload := SlotCommandPayload(cmdSlotRemove, 3)
	code, _ := postJSON(t, h.svc.HandleCallback, eventEnvelope(2000000047, 555, 777, payload))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.sched.removed) != 1 || h.sched.removed[0] != 3 {
		t.Fatalf("removed: %+v", h.sched.removed)
	}
	if len(h.msg.edits) != 1 || h.msg.edits[0].MessageID != 777 {
		t.Fatalf("settings must be rewritten in place: %+v", h.msg.edits)
	}
	if !strings.Contains(h.msg.edits[0].Text, "Убрал: среда") {
		t.Fatalf("note: %q", h.msg.edits[0].Text)
	}
	if len(h.msg.sent) != 0 {
		t.Fatalf("a callback press must not post a message: %+v", h.msg.sent)
	}
}

// --- Слэш-команды ---

// TestParseCommand locks the whole mapping: buttons always work, commands are
// slash-only, ordinary conversation is not a command at all, and the two
// narrow exceptions (connect before the chat is linked, /slot with arguments)
// behave as documented.
func TestParseCommand(t *testing.T) {
	svc := newHarness(t, true).svc

	cases := []struct {
		name        string
		text        string
		payload     string
		connected   bool
		wantCmd     string
		wantEvent   int64
		wantWeekday int
		wantMinutes int
		wantSlot    bool
	}{
		{name: "payload wins over text", text: "что угодно", payload: `{"command":"book","event_id":7}`, connected: true, wantCmd: cmdBook, wantEvent: 7},
		{name: "buttons work even before connect", text: "", payload: `{"command":"book","event_id":7}`, connected: false, wantCmd: cmdBook, wantEvent: 7},

		{name: "slash games", text: "/games", connected: true, wantCmd: cmdGames},
		{name: "slash my", text: "/my", connected: true, wantCmd: cmdMyBookings},
		{name: "slash cancel", text: "/cancel", connected: true, wantCmd: cmdCancelBooking},
		{name: "slash settings", text: "/settings", connected: true, wantCmd: cmdSettings},
		{name: "slash create with args", text: "/create 27.09 19:00", connected: true, wantCmd: cmdCreateGame},
		{name: "slash start russian opens sign-ups", text: "/старт", connected: true, wantCmd: cmdCreateGame},
		{name: "slash latin start is not a command", text: "/start", connected: true, wantCmd: ""},
		{name: "slash connect", text: "/connect", connected: true, wantCmd: cmdConnect},
		{name: "slash help", text: "/помощь", connected: true, wantCmd: cmdStart},
		{name: "slash latin help", text: "/help", connected: true, wantCmd: cmdStart},
		{name: "slash russian", text: "/игры", connected: true, wantCmd: cmdGames},
		{name: "unknown slash", text: "/pizza", connected: true, wantCmd: ""},

		{name: "plain games is conversation", text: "игры", connected: true, wantCmd: ""},
		{name: "plain my is conversation", text: "мои записи", connected: true, wantCmd: ""},
		{name: "plain create is conversation", text: "надо создать игру на выходные", connected: true, wantCmd: ""},
		{name: "plain settings is conversation", text: "какие настройки у бота?", connected: true, wantCmd: ""},
		{name: "plain slot is conversation", text: "вс 10:00", connected: true, wantCmd: ""},
		{name: "plain connect in connected chat", text: "подключить", connected: true, wantCmd: ""},
		{name: "small talk", text: "ребята, кто играет?", connected: true, wantCmd: ""},

		{name: "connect without slash before linking", text: "подключить", connected: false, wantCmd: cmdConnect},
		{name: "slash connect before linking", text: "/connect", connected: false, wantCmd: cmdConnect},
		{name: "nothing else before linking", text: "/games", connected: false, wantCmd: ""},

		{name: "slot by command", text: "/slot вс 10:00", connected: true, wantCmd: cmdSlotAdd, wantWeekday: 0, wantMinutes: 600, wantSlot: true},
		{name: "slot with filler", text: "/slot добавить в воскресенье 10:00", connected: true, wantCmd: cmdSlotAdd, wantWeekday: 0, wantMinutes: 600, wantSlot: true},
		{name: "slot remove", text: "/slot убрать сб", connected: true, wantCmd: cmdSlotRemove, wantWeekday: 6},
		{name: "slot without arguments asks for format", text: "/slot", connected: true, wantCmd: cmdSlotAdd},
	}

	for _, tc := range cases {
		got := svc.parseCommand(tc.text, tc.payload, tc.connected)
		if got.cmd != tc.wantCmd || got.eventID != tc.wantEvent ||
			got.weekday != tc.wantWeekday || got.minutes != tc.wantMinutes || got.hasSlot != tc.wantSlot {
			t.Fatalf("%s: got %+v, want cmd=%q event=%d weekday=%d minutes=%d slot=%v",
				tc.name, got, tc.wantCmd, tc.wantEvent, tc.wantWeekday, tc.wantMinutes, tc.wantSlot)
		}
	}
}

// /create с аргументами: сам слэш-слово не должно попадать ни в дату, ни в
// название игры.
func TestCallbackSlashCreateWithArguments(t *testing.T) {
	h := newHarness(t, true)

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/create 27.09 19:00 8", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.events.created) != 1 {
		t.Fatalf("created: %+v", h.events.created)
	}
	in := h.events.created[0]
	want := time.Date(2026, 9, 27, 19, 0, 0, 0, defaultLocation).UTC()
	if !in.StartsAt.Equal(want) {
		t.Fatalf("starts_at: %v want %v", in.StartsAt.In(defaultLocation), want.In(defaultLocation))
	}
	if in.Title != defaultTitle || in.Capacity != 8 {
		t.Fatalf("input: %+v", in)
	}
}

func TestCallbackSlashHelpShowsCommands(t *testing.T) {
	h := newHarness(t, true)

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/помощь", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	text := h.msg.sent[0].Text
	for _, want := range []string{"/игры", "/мои", "/отмена", "/старт", "/настройки", "/помощь"} {
		if !strings.Contains(text, want) {
			t.Fatalf("help must list %s: %q", want, text)
		}
	}
}

// --- Постоянная клавиатура и payload-объект ---

// VK в Bots Long Poll присылает payload объектом, а не строкой (как описано в
// доке Callback API): обе формы должны разбираться, иначе нажатия кнопок
// теряются.
func TestCallbackObjectPayloadMessageNew(t *testing.T) {
	h := newHarness(t, true)

	envelope := `{"type":"message_new","group_id":12345,"secret":"sekret","object":{"message":{` +
		`"id":9,"date":0,"peer_id":2000000047,"from_id":555,"text":"","out":0,` +
		`"payload":{"command":"my"}}}}`
	code, _ := postJSON(t, h.svc.HandleCallback, envelope)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 || !strings.Contains(h.msg.sent[0].Text, "нет активных записей") {
		t.Fatalf("object payload must be understood: %+v", h.msg.sent)
	}
}

func TestCallbackObjectPayloadMessageEvent(t *testing.T) {
	h := newHarness(t, true)
	h.sched.slots = []schedule.Slot{{ChatID: 1, Weekday: 3, Minutes: 19 * 60}}

	envelope := `{"type":"message_event","group_id":12345,"secret":"sekret","event_id":"e2","object":{` +
		`"user_id":555,"peer_id":2000000047,"event_id":"e2","conversation_message_id":777,` +
		`"payload":{"command":"slot_remove","weekday":3}}}`
	code, _ := postJSON(t, h.svc.HandleCallback, envelope)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.sched.removed) != 1 || h.sched.removed[0] != 3 {
		t.Fatalf("object payload must be understood: %+v", h.sched.removed)
	}
	if len(h.msg.edits) != 1 || h.msg.edits[0].MessageID != 777 {
		t.Fatalf("settings must be rewritten in place: %+v", h.msg.edits)
	}
}

// Клавиатура, которая остаётся под полем ввода: без inline, без one_time и с
// callback-кнопками — нажатие не пишет текст в чат.
func TestPersistentKeyboardShape(t *testing.T) {
	raw, err := persistentKeyboard()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, `"inline"`) {
		t.Fatalf("persistent keyboard must not be inline: %q", raw)
	}
	if strings.Contains(raw, `"one_time"`) {
		t.Fatalf("persistent keyboard must not be one_time: %q", raw)
	}
	if !strings.Contains(raw, `"type":"callback"`) {
		t.Fatalf("persistent buttons must be callback buttons: %q", raw)
	}
	for _, label := range []string{"Иду", "Не иду"} {
		if !strings.Contains(raw, label) {
			t.Fatalf("keyboard must contain %q: %q", label, raw)
		}
	}
}

// Пока открыта игра, любой ответ бота несёт кнопки «Иду» / «Не иду»: иначе
// они пропадали бы из-под поля ввода после первого же сообщения.
func TestRepliesCarrySignUpButtonsWhileGameIsOpen(t *testing.T) {
	h := newHarness(t, true)
	h.events.games = []event.EventSummary{{
		Event: event.Event{ID: 7, Title: "Волейбол", StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12},
	}}

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/мои", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	if !strings.Contains(h.msg.sent[0].Keyboard, "Иду") {
		t.Fatalf("reply must carry the sign-up buttons: %q", h.msg.sent[0].Keyboard)
	}
}

// А когда открытых игр нет, кнопки убираются: пустая клавиатура скрывает их из
// поля ввода, чтобы запись не висела там, где записываться некуда.
func TestRepliesHideSignUpButtonsWhenNoGameIsOpen(t *testing.T) {
	h := newHarness(t, true)

	code, _ := postJSON(t, h.svc.HandleCallback, msgEnvelope(2000000047, 555, "/мои", ""))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if len(h.msg.sent) != 1 {
		t.Fatalf("want 1 reply, got %d", len(h.msg.sent))
	}
	if got := h.msg.sent[0].Keyboard; got != `{"buttons":[]}` {
		t.Fatalf("want an empty keyboard, got %q", got)
	}
}
