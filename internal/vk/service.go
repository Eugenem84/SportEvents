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

// ChatStore resolves and registers external channels in the internal Chat
// model.
type ChatStore interface {
	FindByChannel(ctx context.Context, platform, externalChatID string) (*chat.Chat, error)
	// Connect binds the external conversation to an internal Chat and
	// returns it with created=false when it was already connected.
	Connect(ctx context.Context, in chat.ConnectInput) (chat.Chat, bool, error)
}

// ConversationInfo reads the VK conversation metadata needed at connect
// time (title, initiator rights). It is separate from Messenger because it
// is used only when a conversation is registered.
type ConversationInfo interface {
	// GetConversationTitle returns the title of a group conversation.
	GetConversationTitle(ctx context.Context, peerID int64) (string, error)
	// IsConversationAdmin reports whether userID administers the
	// conversation (messages.getConversationMembers).
	IsConversationAdmin(ctx context.Context, peerID, userID int64) (bool, error)
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
	convs     ConversationInfo

	// events deduplicates VK callback events by their event_id, so a retry of
	// the same event is not processed twice.
	events *eventDedup

	log *log.Logger
}

func NewService(cfg Config, messenger Messenger, chats ChatStore, users UserStore, convs ConversationInfo) *Service {
	gid, _ := strconv.ParseInt(strings.TrimSpace(cfg.GroupID), 10, 64)
	return &Service{
		confirmationToken: strings.TrimSpace(cfg.ConfirmationToken),
		secret:            cfg.Secret,
		groupID:           gid,
		messenger:         messenger,
		chats:             chats,
		users:             users,
		convs:             convs,
		events:            newEventDedup(callbackDedupTTL),
		log:               log.Default(),
	}
}

const (
	cmdStart         = "start"
	cmdGames         = "games"
	cmdMyBookings    = "my"
	cmdCancelBooking = "cancel"
	cmdConnect       = "connect"
)

// conversationPeerIDMin is the lowest peer_id of a VK group conversation.
// One-to-one dialogs use a plain user id below it, and a dialog has no
// admins to register, so only group conversations can be connected.
const conversationPeerIDMin = 2_000_000_000

func isConversation(peerID int64) bool { return peerID >= conversationPeerIDMin }

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
	cmd := s.commandOf(msg.Text, msg.Payload)

	// An unconnected conversation is not a Chat yet: the connection
	// request is the only command that can act on it.
	if ch == nil {
		if cmd == cmdConnect {
			return s.handleConnect(ctx, msg)
		}
		s.log.Printf("vk: message from unconnected peer %d", msg.PeerID)
		return s.sendText(ctx, msg.PeerID, "Эта беседа ещё не подключена. Напишите «подключить», чтобы подключить её.", "")
	}

	user, err := s.ensureUser(ctx, msg.FromID)
	if err != nil {
		return err
	}

	switch cmd {
	case cmdStart:
		return s.sendWelcome(ctx, msg.PeerID, user.DisplayName, ch.Title)
	case cmdConnect:
		return s.sendText(ctx, msg.PeerID, fmt.Sprintf("Беседа «%s» уже подключена.", ch.Title), "")
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

// handleConnect registers the conversation as an internal Chat. Only a VK
// conversation administrator may connect it, and only a group conversation:
// a one-to-one dialog has no admins to speak of. The initiator becomes the
// first ChatAdmin. Rights are checked against the VK API here, at connect
// time; afterwards they are read from chat_admins.
func (s *Service) handleConnect(ctx context.Context, msg messageNew) error {
	if !isConversation(msg.PeerID) {
		return s.sendText(ctx, msg.PeerID,
			"Подключить можно только беседу: добавьте бота в беседу и напишите «подключить».", "")
	}

	user, err := s.ensureUser(ctx, msg.FromID)
	if err != nil {
		return err
	}

	isAdmin, err := s.convs.IsConversationAdmin(ctx, msg.PeerID, msg.FromID)
	if err != nil {
		return fmt.Errorf("check conversation admin: %w", err)
	}
	if !isAdmin {
		s.log.Printf("vk: connect denied for peer %d: user %d is not an admin", msg.PeerID, msg.FromID)
		return s.sendText(ctx, msg.PeerID, "Подключить беседу может только её администратор.", "")
	}

	title, err := s.convs.GetConversationTitle(ctx, msg.PeerID)
	if err != nil || strings.TrimSpace(title) == "" {
		s.log.Printf("vk: conversation title unavailable for %d: %v", msg.PeerID, err)
		title = fmt.Sprintf("Беседа %d", msg.PeerID)
	}

	ch, created, err := s.chats.Connect(ctx, chat.ConnectInput{
		Platform:       chat.PlatformVK,
		ExternalChatID: strconv.FormatInt(msg.PeerID, 10),
		ChatTitle:      title,
		InitiatorID:    user.ID,
	})
	if err != nil {
		return fmt.Errorf("connect chat: %w", err)
	}
	if !created {
		return s.sendText(ctx, msg.PeerID, fmt.Sprintf("Беседа «%s» уже подключена.", ch.Title), "")
	}

	kb, err := defaultKeyboard()
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Беседа «%s» подключена.\n%s — администратор.\nКоманды: «Игры», «Мои записи», «Отмена записи».",
		ch.Title, user.DisplayName)
	return s.sendText(ctx, msg.PeerID, text, kb)
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
	case strings.Contains(t, "подключ"):
		return cmdConnect
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
