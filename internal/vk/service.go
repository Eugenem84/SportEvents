package vk

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"sportevents.local/internal/chat"
)

// Messenger talks to the VK Bot API. *Client implements it.
type Messenger interface {
	SendMessage(ctx context.Context, peerID int64, text, keyboard string) (int64, error)
	// AnswerMessageEvent acknowledges a callback-button press so the pressed
	// button stops showing the loading state in VK clients. eventData is the
	// optional JSON action to run on the client (e.g. a toast) and may be
	// empty.
	AnswerMessageEvent(ctx context.Context, userID, peerID int64, eventID, eventData string) error
	GetUserName(ctx context.Context, userID int64) (string, error)
}

// ChatStore resolves an external channel to the internal Chat.
type ChatStore interface {
	FindByChannel(ctx context.Context, platform, externalChatID string) (*chat.Chat, error)
}

// UserStore resolves an external identity to the internal User.
type UserStore interface {
	FindOrCreateUserByExternalID(ctx context.Context, platform, externalUserID, displayName string) (chat.User, error)
}

// Config carries the community settings for the callback endpoint.
type Config struct {
	// ConfirmationToken is the string VK expects back for a
	// "confirmation" event (from the community Callback API settings).
	ConfirmationToken string
	// Secret is the optional callback secret key; when set, every event
	// carrying a different secret is rejected.
	Secret string
	// GroupID is the community id; when non-empty, events from other
	// communities are rejected.
	GroupID string
}

// Service handles VK Callback API events. It depends on interfaces only,
// so it is testable without PostgreSQL or the real VK API.
type Service struct {
	confirmationToken string
	secret            string
	groupID           int64

	messenger Messenger
	chats     ChatStore
	users     UserStore

	// events deduplicates VK callback events by their event_id, so a retry of
	// the same event is not processed twice.
	events *eventDedup

	log *log.Logger
}

func NewService(cfg Config, messenger Messenger, chats ChatStore, users UserStore) *Service {
	gid, _ := strconv.ParseInt(strings.TrimSpace(cfg.GroupID), 10, 64)
	return &Service{
		confirmationToken: strings.TrimSpace(cfg.ConfirmationToken),
		secret:            cfg.Secret,
		groupID:           gid,
		messenger:         messenger,
		chats:             chats,
		users:             users,
		events:            newEventDedup(callbackDedupTTL),
		log:               log.Default(),
	}
}

const (
	cmdStart         = "start"
	cmdGames         = "games"
	cmdMyBookings    = "my"
	cmdCancelBooking = "cancel"
)

// handleMessageNew routes a conversation message. from_id / peer_id are
// resolved to an internal User and Chat; the identity is created on first
// contact (idempotent), so a redelivered event cannot duplicate it.
func (s *Service) handleMessageNew(ctx context.Context, msg messageNew) error {
	if msg.Out == 1 {
		return nil // our own outgoing message, skip
	}

	peerID := strconv.FormatInt(msg.PeerID, 10)
	ch, err := s.chats.FindByChannel(ctx, chat.PlatformVK, peerID)
	if err != nil {
		return fmt.Errorf("find chat: %w", err)
	}
	if ch == nil {
		s.log.Printf("vk: message from unconnected peer %d", msg.PeerID)
		return s.sendText(ctx, msg.PeerID, "Эта беседа ещё не подключена. Подключение появится в следующей версии бота.", "")
	}

	user, err := s.ensureUser(ctx, msg.FromID)
	if err != nil {
		return err
	}

	switch s.commandOf(msg.Text, msg.Payload) {
	case cmdStart:
		return s.sendWelcome(ctx, msg.PeerID, user.DisplayName, ch.Title)
	case cmdGames:
		return s.sendText(ctx, msg.PeerID, "Список игр появится на следующем этапе.", "")
	case cmdMyBookings:
		return s.sendText(ctx, msg.PeerID, "Ваши записи появятся на следующем этапе.", "")
	case cmdCancelBooking:
		return s.sendText(ctx, msg.PeerID, "Отмена записи появится на следующем этапе.", "")
	default:
		return s.sendText(ctx, msg.PeerID, "Не понял команду. Напишите /start.", "")
	}
}

// handleMessageEvent routes a callback-button press. VK delivers it as a
// message_event object with user_id, peer_id and the button payload.
func (s *Service) handleMessageEvent(ctx context.Context, ev messageEvent) error {
	// A pressed callback button keeps showing "processing…" until the bot
	// answers the event, so acknowledge it first — even when the lookup
	// below fails and no text reply follows.
	if err := s.messenger.AnswerMessageEvent(ctx, ev.UserID, ev.PeerID, ev.EventID, ""); err != nil {
		s.log.Printf("vk: answer message_event: %v", err)
	}

	peerID := strconv.FormatInt(ev.PeerID, 10)
	ch, err := s.chats.FindByChannel(ctx, chat.PlatformVK, peerID)
	if err != nil {
		return fmt.Errorf("find chat: %w", err)
	}
	if ch == nil {
		s.log.Printf("vk: event from unconnected peer %d", ev.PeerID)
		return nil
	}

	user, err := s.ensureUser(ctx, ev.UserID)
	if err != nil {
		return err
	}

	switch s.commandOf("", ev.Payload) {
	case cmdStart:
		return s.sendWelcome(ctx, ev.PeerID, user.DisplayName, ch.Title)
	case cmdGames:
		return s.sendText(ctx, ev.PeerID, "Список игр появится на следующем этапе.", "")
	case cmdMyBookings:
		return s.sendText(ctx, ev.PeerID, "Ваши записи появятся на следующем этапе.", "")
	case cmdCancelBooking:
		return s.sendText(ctx, ev.PeerID, "Отмена записи появится на следующем этапе.", "")
	default:
		return s.sendText(ctx, ev.PeerID, "Не понял команду. Напишите /start.", "")
	}
}

// ensureUser resolves the VK user to an internal User, creating the
// identity on first contact. A VK API failure to fetch the display name is
// not fatal: the fallback id still identifies the user unambiguously.
func (s *Service) ensureUser(ctx context.Context, vkID int64) (chat.User, error) {
	name := fmt.Sprintf("vk%d", vkID)
	if got, err := s.messenger.GetUserName(ctx, vkID); err == nil && got != "" {
		name = got
	} else if err != nil {
		s.log.Printf("vk: users.get failed for %d, using fallback: %v", vkID, err)
	}
	return s.users.FindOrCreateUserByExternalID(ctx, chat.PlatformVK, strconv.FormatInt(vkID, 10), name)
}

func (s *Service) sendText(ctx context.Context, peerID int64, text, keyboard string) error {
	if _, err := s.messenger.SendMessage(ctx, peerID, text, keyboard); err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	return nil
}

func (s *Service) sendWelcome(ctx context.Context, peerID int64, displayName, chatTitle string) error {
	kb, err := defaultKeyboard()
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Привет, %s!\nЭто бот записи на игры чата «%s».\nКоманды появятся на следующих этапах.", displayName, chatTitle)
	return s.sendText(ctx, peerID, text, kb)
}

// defaultKeyboard is the placeholder keyboard for Phase 4. The buttons
// send text commands; real booking actions arrive in later phases.
func defaultKeyboard() (string, error) {
	kb := Keyboard{
		Inline: true,
		Buttons: [][]Button{
			{
				TextButton("Игры", CommandPayload(cmdGames), ColorPrimary),
				TextButton("Мои записи", CommandPayload(cmdMyBookings), ColorSecondary),
			},
			{
				TextButton("Отмена записи", CommandPayload(cmdCancelBooking), ColorNegative),
			},
		},
	}
	return kb.Marshal()
}

// commandOf maps a message text and/or a button payload to a command.
func (s *Service) commandOf(text, payload string) string {
	if cmd := PayloadCommand(payload); cmd != "" {
		return cmd
	}
	t := strings.ToLower(strings.TrimSpace(text))
	switch {
	case t == "/start" || t == "start" || t == "начать":
		return cmdStart
	case strings.Contains(t, "игр"):
		return cmdGames
	case strings.Contains(t, "запис"):
		return cmdMyBookings
	case strings.Contains(t, "отмен"):
		return cmdCancelBooking
	}
	return ""
}

const (
	// callbackDedupTTL is how long a processed VK event id is remembered.
	// VK retries the same event for the better part of an hour, so an hour
	// window covers those retries without growing the map unboundedly.
	callbackDedupTTL = time.Hour
	// callbackDedupMax is the size at which expired entries are swept. It
	// bounds memory for busy communities; the community-wide bot stays far
	// below it in practice.
	callbackDedupMax = 8192
)

// eventDedup remembers recently handled VK callback event ids. VK retries
// events it did not receive a confident answer for, and the same event id
// must not be processed twice (two welcome messages, two identity lookups).
// It is strictly an in-memory optimisation: PostgreSQL unique indexes
// already make the state changes under it (user identity, active booking)
// idempotent, so a duplicate that slips through after a restart can only
// trigger a duplicate reply, never duplicate data.
type eventDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
}

func newEventDedup(ttl time.Duration) *eventDedup {
	return &eventDedup{seen: make(map[string]time.Time), ttl: ttl}
}

// mark records eid and reports whether it had already been seen within ttl.
func (d *eventDedup) mark(eid string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if exp, ok := d.seen[eid]; ok && now.Before(exp) {
		return true
	}

	if len(d.seen) >= callbackDedupMax {
		for k, exp := range d.seen {
			if !now.Before(exp) {
				delete(d.seen, k)
			}
		}
	}
	d.seen[eid] = now.Add(d.ttl)
	return false
}
