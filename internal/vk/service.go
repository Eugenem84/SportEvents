package vk

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"sportevents.local/internal/announce"
	"sportevents.local/internal/booking"
	"sportevents.local/internal/chat"
	"sportevents.local/internal/event"
	"sportevents.local/internal/schedule"
)

// Messenger talks to the VK Bot API. *Client implements it.
type Messenger interface {
	SendMessage(ctx context.Context, peerID int64, text, keyboard string) (int64, error)
	// EditMessage rewrites a message the community sent earlier: the game
	// announcement keeps the roster of participants in one message instead of
	// posting a new one on every booking.
	EditMessage(ctx context.Context, peerID, messageID int64, text, keyboard string) (int64, error)
	// LastOwnConversationMessageID names the id of the last message the
	// community posted to a conversation. В беседе messages.send отвечает 0,
	// поэтому id только что отправленного анонса читается из самой беседы.
	LastOwnConversationMessageID(ctx context.Context, peerID int64) (int64, error)
	// PinMessage pins a message at the top of the conversation, so the
	// announcement with the roster does not scroll away.
	PinMessage(ctx context.Context, peerID, messageID int64) error
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
	// IsChatAdmin reports whether the user administers the Chat. Rights are
	// read from chat_admins; the VK API is asked only at connect time.
	IsChatAdmin(ctx context.Context, chatID, userID int64) (bool, error)
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

// EventStore is the event side of the bot: creating games and listing them
// with their counters.
type EventStore interface {
	Create(ctx context.Context, in event.CreateInput) (event.Event, error)
	Get(ctx context.Context, chatID, eventID int64) (event.Event, error)
	ListUpcomingWithCounts(ctx context.Context, chatID int64, from time.Time, limit int) ([]event.EventSummary, error)
}

// BookingStore is the booking side of the bot: who is in the lineup, who is
// in the reserve, and what a cancellation did to them.
type BookingStore interface {
	Create(ctx context.Context, in booking.CreateInput) (booking.Booking, error)
	Cancel(ctx context.Context, eventID, bookingID int64) (booking.CancelResult, error)
	ListByEvent(ctx context.Context, eventID int64, statuses ...booking.Status) ([]booking.Booking, error)
	ListActiveByUser(ctx context.Context, userID int64, from time.Time) ([]booking.BookingWithEvent, error)
}

// AnnounceStore remembers the chat message that announces a game.
type AnnounceStore interface {
	Save(ctx context.Context, ref announce.Ref) error
	Get(ctx context.Context, eventID int64, platform string) (announce.Ref, bool, error)
}

// ScheduleStore holds the regular game times of a chat. The admin sets them
// once, and "создать игру" then needs no date at all.
type ScheduleStore interface {
	List(ctx context.Context, chatID int64) ([]schedule.Slot, error)
	Set(ctx context.Context, chatID int64, weekday, minutes int) (schedule.Slot, error)
	Delete(ctx context.Context, chatID int64, weekday int) (bool, error)
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
	// Location is the timezone games are shown in. Times are stored in UTC;
	// V1 renders them in one fixed zone per chat (ARCHITECTURE §7), and a
	// fixed offset keeps the bot working in a container without tzdata.
	Location *time.Location
}

// Deps are the collaborators of the VK adapter. A struct keeps NewService
// readable now that the bot also talks to events and bookings.
type Deps struct {
	Messenger Messenger
	Chats     ChatStore
	Users     UserStore
	Convs     ConversationInfo
	Events    EventStore
	Bookings  BookingStore
	Announces AnnounceStore
	Schedule  ScheduleStore
	// Now returns the current time. Tests pin it, so "tomorrow 19:00" means
	// something in assertions.
	Now func() time.Time
}

// defaultLocation is Moscow time: V1 assumes one city per chat.
var defaultLocation = time.FixedZone("MSK", 3*60*60)

// Service handles VK Callback API events. It depends on interfaces only,
// so it is testable without PostgreSQL or the real VK API.
type Service struct {
	confirmationToken string
	secret            string
	groupID           int64
	loc               *time.Location

	messenger Messenger
	chats     ChatStore
	users     UserStore
	convs     ConversationInfo
	events    EventStore
	bookings  BookingStore
	announces AnnounceStore
	schedule  ScheduleStore
	now       func() time.Time

	// dedup remembers recently handled VK event ids, so a retry of the same
	// event is not processed twice.
	dedup *eventDedup

	log *log.Logger
}

func NewService(cfg Config, deps Deps) *Service {
	gid, _ := strconv.ParseInt(strings.TrimSpace(cfg.GroupID), 10, 64)
	loc := cfg.Location
	if loc == nil {
		loc = defaultLocation
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		confirmationToken: strings.TrimSpace(cfg.ConfirmationToken),
		secret:            cfg.Secret,
		groupID:           gid,
		loc:               loc,
		messenger:         deps.Messenger,
		chats:             deps.Chats,
		users:             deps.Users,
		convs:             deps.Convs,
		events:            deps.Events,
		bookings:          deps.Bookings,
		announces:         deps.Announces,
		schedule:          deps.Schedule,
		now:               now,
		dedup:             newEventDedup(callbackDedupTTL),
		log:               log.Default(),
	}
}

const (
	cmdStart         = "start"
	cmdGames         = "games"
	cmdMyBookings    = "my"
	cmdCancelBooking = "cancel"
	cmdConnect       = "connect"
	// cmdCreateGame creates a game and posts its announcement (admins only).
	cmdCreateGame = "create"
	// cmdAttend is the "Иду" button: sign up for the nearest game (or for the
	// game in the payload).
	cmdAttend = "attend"
	// cmdBook is the old name of cmdAttend: buttons already sitting in chats
	// keep working.
	cmdBook = "book"
	// cmdSkip is the "Не иду" button: cancel the caller's booking.
	cmdSkip = "skip"
	// cmdSettings opens the schedule screen of the chat (admins only).
	cmdSettings = "settings"
	// cmdSlotAdd and cmdSlotRemove edit one weekday of that schedule:
	// cmdSlotAdd without parameters only explains the expected input.
	cmdSlotAdd    = "slot_add"
	cmdSlotRemove = "slot_remove"
)

// settingsRefusal and gamesRefusal answer members who are not chat admins.
const (
	settingsRefusal = "Настройки расписания может менять только администратор беседы."
	gamesRefusal    = "Создавать игры может только администратор беседы."
)

const (
	// gamesLimit caps the "игры" list: a chat screen shows a handful of games,
	// not the whole season.
	gamesLimit = 10
	// defaultCapacity, defaultTitle and defaultHour are what "создать игру"
	// without arguments produces.
	defaultCapacity = 12
	defaultTitle    = "Волейбол"
	defaultHour     = 19
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
	parsed := s.parseCommand(msg.Text, string(msg.Payload), ch != nil)

	// An unconnected conversation is not a Chat yet: only the connection
	// request makes sense there, the rest is people talking.
	if ch == nil {
		if parsed.cmd == cmdConnect {
			return s.handleConnect(ctx, msg)
		}
		return nil
	}

	// Не команда — это обычная переписка в беседе. Бот молчит и даже не
	// заводит identity: сообщения людей — не его дело.
	if parsed.cmd == "" {
		return nil
	}

	user, err := s.ensureUser(ctx, msg.FromID)
	if err != nil {
		return err
	}
	return s.handleCommand(ctx, &commandCtx{
		peerID:  msg.PeerID,
		chat:    *ch,
		user:    user,
		cmd:     parsed.cmd,
		eventID: parsed.eventID,
		weekday: parsed.weekday,
		minutes: parsed.minutes,
		hasSlot: parsed.hasSlot,
		text:    stripCommandWord(msg.Text),
	})
}

// commandCtx is everything one command needs: the external peer, the internal
// Chat and User it belongs to, and the parsed command with its parameters.
type commandCtx struct {
	peerID  int64
	chat    chat.Chat
	user    chat.User
	cmd     string
	eventID int64
	weekday int
	minutes int
	hasSlot bool
	text    string
	// messageID is the message this interaction must rewrite: callback
	// buttons carry the id of the message they were pressed on. Zero means
	// "post a new message".
	messageID int64
	// vkUserID and vkEventID come from a button press (message_event): the
	// event id is what the bot answers with a personal snackbar, and the user
	// id is whom it belongs to.
	vkUserID  int64
	vkEventID string
	// answered becomes true once the button press has been answered.
	answered bool
}

// parsedCommand is a command with the parameters it came with.
type parsedCommand struct {
	cmd     string
	eventID int64
	weekday int
	minutes int
	hasSlot bool
}

// handleCommand runs one command. Both message_new (typed text and text
// buttons) and message_event (callback buttons) end up here, so the two
// transports cannot drift apart.
func (s *Service) handleCommand(ctx context.Context, c *commandCtx) error {
	switch c.cmd {
	case cmdStart:
		return s.sendWelcome(ctx, c.peerID, c.user.DisplayName, c.chat.Title)
	case cmdConnect:
		return s.sendText(ctx, c.peerID, fmt.Sprintf("Беседа «%s» уже подключена.", c.chat.Title), "")
	case cmdGames:
		return s.handleGames(ctx, c)
	case cmdCreateGame:
		return s.handleCreateGame(ctx, c)
	case cmdBook, cmdAttend:
		return s.handleAttend(ctx, c)
	case cmdSkip:
		return s.handleSkip(ctx, c)
	case cmdMyBookings:
		return s.handleMyBookings(ctx, c)
	case cmdCancelBooking:
		return s.handleCancel(ctx, c)
	case cmdSettings:
		return s.handleSettings(ctx, c)
	case cmdSlotAdd:
		return s.handleSlotAdd(ctx, c)
	case cmdSlotRemove:
		return s.handleSlotRemove(ctx, c)
	default:
		// Неизвестная команда: молчим, чтобы не мешать переписке в беседе.
		return nil
	}
}

// handleMessageEvent routes a callback-button press. VK delivers it as a
// message_event object with user_id, peer_id and the button payload.
func (s *Service) handleMessageEvent(ctx context.Context, ev messageEvent) error {
	// A pressed callback button keeps showing "processing…" until the bot
	// answers its event. Handlers answer with a personal snackbar; when one
	// does not — or when we bail out early — an empty answer stops the
	// spinner. Either way the press is answered exactly once.
	answered := false
	defer func() {
		if answered {
			return
		}
		if err := s.messenger.AnswerMessageEvent(ctx, ev.UserID, ev.PeerID, ev.EventID, ""); err != nil {
			s.log.Printf("vk: answer message_event: %v", err)
		}
	}()

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

	parsed := ParseButtonPayload(ev.Payload.String())

	c := &commandCtx{
		peerID:    ev.PeerID,
		chat:      *ch,
		user:      user,
		cmd:       parsed.Command,
		eventID:   parsed.EventID,
		weekday:   parsed.Weekday,
		messageID: ev.ConversationMessageID,
		vkUserID:  ev.UserID,
		vkEventID: ev.EventID,
	}
	// Нажатие на кнопку самого анонса — единственный способ узнать id этого
	// сообщения в беседе: messages.send отвечает там 0. Запоминаем его, чтобы
	// дальше переписывать анонс на месте.
	s.rememberAnnouncementID(ctx, ev.PeerID, parsed, ev.ConversationMessageID)
	err = s.handleCommand(ctx, c)
	answered = c.answered
	return err
}

// rememberAnnouncementID fills in the id of the announcement message once a
// press on the announcement itself reports it. Nothing is overwritten: only a
// reference whose id is still unknown learns it, and only from the chat that
// reference belongs to.
func (s *Service) rememberAnnouncementID(ctx context.Context, peerID int64, p Payload, messageID int64) {
	if !p.Announce || p.EventID == 0 || messageID == 0 {
		return
	}
	ref, ok, err := s.announces.Get(ctx, p.EventID, chat.PlatformVK)
	if err != nil {
		s.log.Printf("vk: get announcement of game %d: %v", p.EventID, err)
		return
	}
	external := strconv.FormatInt(peerID, 10)
	if !ok || ref.MessageID != 0 || ref.ExternalChatID != external {
		return
	}
	ref.MessageID = messageID
	if err := s.announces.Save(ctx, ref); err != nil {
		s.log.Printf("vk: remember announcement message of game %d: %v", p.EventID, err)
		return
	}

	// Только теперь анонс можно закрепить: при отправке VK не называет id
	// сообщения в беседе. Закрепление разрешено владельцу беседы, остальным VK
	// отвечает 925 — это не ошибка записи, поэтому просто пишем в лог.
	if err := s.messenger.PinMessage(ctx, peerID, messageID); err != nil {
		s.log.Printf("vk: cannot pin announcement of game %d: %v", p.EventID, err)
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
		// VK refusing to list members (e.g. the bot is not an admin of the
		// chat) is an external failure the user can act on: answer instead
		// of staying silent, and keep the real error in the log.
		s.log.Printf("vk: cannot check admins of peer %d: %v", msg.PeerID, err)
		return s.sendText(ctx, msg.PeerID,
			"Не удалось проверить права в беседе. Убедитесь, что бот добавлен в беседу и назначен администратором, и попробуйте ещё раз.", "")
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

	kb, err := persistentKeyboard()
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

// handleGames lists the upcoming games of the chat with their free slots and
// a button per game: signing up is one press from here.
func (s *Service) handleGames(ctx context.Context, c *commandCtx) error {
	games, err := s.events.ListUpcomingWithCounts(ctx, c.chat.ID, s.now().UTC(), gamesLimit)
	if err != nil {
		return fmt.Errorf("list games: %w", err)
	}
	if len(games) == 0 {
		return s.sendText(ctx, c.peerID,
			"Ближайших игр нет. Администратор может создать игру: «создать игру 27.09 19:00 12».", "")
	}

	var b strings.Builder
	b.WriteString("Ближайшие игры:\n")
	rows := make([][]Button, 0, len(games))
	for i, g := range games {
		fmt.Fprintf(&b, "\n%d) %s — %s\n", i+1, g.Title, s.formatWhenShort(g.StartsAt))
		fmt.Fprintf(&b, "   свободно %d из %d", g.Free, g.Capacity)
		if g.Waitlist > 0 {
			fmt.Fprintf(&b, ", в резерве %d", g.Waitlist)
		}
		b.WriteString("\n")
		rows = append(rows, []Button{
			CallbackButton("Иду: "+g.Title, EventCommandPayload(cmdAttend, g.ID), ColorPositive),
		})
	}
	kb, err := (Keyboard{Inline: true, Buttons: rows}).Marshal()
	if err != nil {
		return err
	}
	return s.sendText(ctx, c.peerID, b.String(), kb)
}

// handleCreateGame creates a game and posts the announcement the whole chat
// signs up under. Only a chat administrator may do it. The date comes from
// the chat schedule unless the admin typed one.
func (s *Service) handleCreateGame(ctx context.Context, c *commandCtx) error {
	ok, err := s.requireAdmin(ctx, c, gamesRefusal)
	if err != nil || !ok {
		return err
	}

	draft := parseCreateGame(c.text, s.now(), s.loc)
	startsAt, err := s.gameStart(ctx, c.chat.ID, draft)
	if err != nil {
		return err
	}

	// A scheduled game is created by a single press, so a second press must
	// not announce the same game twice: the existing announcement is simply
	// posted again, with the current roster.
	if existing, ok, err := s.findGameAt(ctx, c.chat.ID, startsAt); err != nil {
		return err
	} else if ok {
		if err := s.announceGame(ctx, c, existing.Event); err != nil {
			return err
		}
		return s.sendText(ctx, c.peerID, fmt.Sprintf(
			"Игра на %s уже создана — обновил анонс с составом.", s.formatWhenShort(existing.StartsAt)), "")
	}

	ev, err := s.events.Create(ctx, event.CreateInput{
		ChatID:   c.chat.ID,
		StartsAt: startsAt,
		Title:    draft.title,
		Location: draft.location,
		Capacity: draft.capacity,
	})
	if err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	return s.announceGame(ctx, c, ev)
}

// announceGame posts the announcement everyone signs up under and remembers
// where it is. VK answers messages.send with 0 in a conversation, so the id
// stays unknown until someone presses a button of the announcement itself;
// until then the roster is not rewritten in place.
func (s *Service) announceGame(ctx context.Context, c *commandCtx, ev event.Event) error {
	confirmed, waitlist, err := s.roster(ctx, ev.ID)
	if err != nil {
		return err
	}

	text, kb, err := s.renderAnnouncement(ev, confirmed, waitlist)
	if err != nil {
		return err
	}
	msgID, err := s.messenger.SendMessage(ctx, c.peerID, text, kb)
	if err != nil {
		return fmt.Errorf("send announcement: %w", err)
	}
	if msgID == 0 {
		// VK не называет id сообщения в беседе: читаем id последнего сообщения
		// беседы — только что отправленный анонс и есть последнее.
		last, err := s.messenger.LastOwnConversationMessageID(ctx, c.peerID)
		switch {
		case err != nil:
			s.log.Printf("vk: cannot read the announcement id of game %d: %v", ev.ID, err)
		default:
			msgID = last
		}
	}
	if err := s.announces.Save(ctx, announce.Ref{
		EventID:        ev.ID,
		Platform:       chat.PlatformVK,
		ExternalChatID: strconv.FormatInt(c.peerID, 10),
		MessageID:      msgID,
	}); err != nil {
		return fmt.Errorf("save announcement: %w", err)
	}

	// Пробуем закрепить анонс, чтобы состав не уезжал вверх вместе с
	// перепиской. VK разрешает это владельцу беседы; сообществу, которое в
	// беседе лишь администратор, он отвечает 925 — тогда просто живём дальше.
	if msgID != 0 {
		if err := s.messenger.PinMessage(ctx, c.peerID, msgID); err != nil {
			s.log.Printf("vk: cannot pin announcement of game %d: %v", ev.ID, err)
		}
	}
	return nil
}

// roster splits the bookings of an event into the lineup and the reserve.
func (s *Service) roster(ctx context.Context, eventID int64) (confirmed, waitlist []booking.Booking, err error) {
	all, err := s.bookings.ListByEvent(ctx, eventID, booking.StatusConfirmed, booking.StatusWaitlist)
	if err != nil {
		return nil, nil, fmt.Errorf("list bookings: %w", err)
	}
	for _, b := range all {
		if b.Status == booking.StatusConfirmed {
			confirmed = append(confirmed, b)
		} else {
			waitlist = append(waitlist, b)
		}
	}
	return confirmed, waitlist, nil
}

// gameStart decides when the game starts: an explicit date in the text wins,
// then an explicit weekday, then the chat schedule, and only then the built-in
// default (tomorrow 19:00, already in draft.startsAt).
func (s *Service) gameStart(ctx context.Context, chatID int64, draft gameDraft) (time.Time, error) {
	if draft.dateSet {
		return draft.startsAt, nil
	}

	if draft.weekday >= 0 {
		local := draft.startsAt.In(s.loc)
		minutes := local.Hour()*60 + local.Minute()
		if at, ok := schedule.Next(s.now(), []schedule.Slot{{Weekday: draft.weekday, Minutes: minutes}}, s.loc); ok {
			return at, nil
		}
	}

	if s.schedule != nil {
		slots, err := s.schedule.List(ctx, chatID)
		if err != nil {
			return time.Time{}, fmt.Errorf("list schedule: %w", err)
		}
		if at, ok := schedule.Next(s.now(), slots, s.loc); ok {
			return at, nil
		}
	}
	return draft.startsAt, nil
}

// findGameAt reports whether the chat already has a game starting at t.
func (s *Service) findGameAt(ctx context.Context, chatID int64, t time.Time) (event.EventSummary, bool, error) {
	games, err := s.events.ListUpcomingWithCounts(ctx, chatID, s.now().UTC(), gamesLimit)
	if err != nil {
		return event.EventSummary{}, false, fmt.Errorf("list games: %w", err)
	}
	for _, g := range games {
		if g.StartsAt.Equal(t) {
			return g, true, nil
		}
	}
	return event.EventSummary{}, false, nil
}

// renderAnnouncement renders the game message everyone signs up under: the date
// with its weekday, the lineup with seat numbers (a freed seat shows as
// «свободно», so the numbering never shifts), the free-slot counter and the
// reserve in queue order. The buttons act on this very game.
func (s *Service) renderAnnouncement(ev event.Event, confirmed, waitlist []booking.Booking) (string, string, error) {
	bySeat := make(map[int]string, len(confirmed))
	for _, bk := range confirmed {
		if bk.SeatNo != nil {
			bySeat[*bk.SeatNo] = bk.PlayerName
		}
	}

	free := ev.Capacity - len(confirmed)
	if free < 0 {
		free = 0
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", ev.Title, s.formatWhenFull(ev.StartsAt))
	if strings.TrimSpace(ev.Location) != "" {
		fmt.Fprintf(&b, "Место: %s\n", ev.Location)
	}
	fmt.Fprintf(&b, "\nСостав (%d/%d):\n", len(confirmed), ev.Capacity)
	if len(confirmed) == 0 {
		b.WriteString("— пока никого\n")
	}
	for seat := 1; seat <= ev.Capacity; seat++ {
		if seat > 1 {
			b.WriteString(" · ")
		}
		name, taken := bySeat[seat]
		if !taken {
			name = "свободно"
		}
		fmt.Fprintf(&b, "%d. %s", seat, name)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Свободно мест: %d\n", free)

	if len(waitlist) > 0 {
		fmt.Fprintf(&b, "\nРезерв (%d): %s\n", len(waitlist), reserveLine(waitlist))
	}

	// Кнопки анонса несут признак announce: их нажатие сообщает id самого
	// анонса, без этого бот не смог бы переписать его на месте (в беседе
	// messages.send отвечает 0 вместо id).
	kb := Keyboard{Inline: true, Buttons: [][]Button{
		{CallbackButton("Иду", AnnouncementCommandPayload(cmdAttend, ev.ID), ColorPositive)},
		{CallbackButton("Не иду", AnnouncementCommandPayload(cmdSkip, ev.ID), ColorNegative)},
	}}
	raw, err := kb.Marshal()
	if err != nil {
		return "", "", err
	}
	return b.String(), raw, nil
}

// monthsGenitive names the months the way the announcement reads them
// («27 сентября»).
var monthsGenitive = [12]string{
	"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря",
}

// formatWhenFull renders a game time in full: «воскресенье, 27 сентября, 10:00».
// An absolute date beats «сегодня/завтра» for a message that stays pinned in the
// chat for days.
func (s *Service) formatWhenFull(t time.Time) string {
	local := t.In(s.loc)
	return fmt.Sprintf("%s, %d %s, %s",
		schedule.LongWeekday(int(local.Weekday())),
		local.Day(),
		monthsGenitive[local.Month()-1],
		local.Format("15:04"),
	)
}

// formatWhenShort renders a compact game time for lists: «вс, 27.09, 10:00».
func (s *Service) formatWhenShort(t time.Time) string {
	local := t.In(s.loc)
	return fmt.Sprintf("%s, %s, %s",
		schedule.ShortWeekday(int(local.Weekday())),
		local.Format("02.01"),
		local.Format("15:04"),
	)
}

// reserveLine renders the reserve in queue order: first in line goes first.
func reserveLine(waitlist []booking.Booking) string {
	names := make([]string, 0, len(waitlist))
	for _, b := range waitlist {
		names = append(names, b.PlayerName)
	}
	return strings.Join(names, " · ")
}

// handleAttend is the "Иду" button: it signs the caller up for the game in the
// payload, or for the nearest upcoming game when the press came from the
// persistent keyboard. booking.Create decides between the lineup and the
// reserve. The chat gets a short line with the seat number and the name, the
// person gets a personal snackbar.
func (s *Service) handleAttend(ctx context.Context, c *commandCtx) error {
	ev, ok, err := s.targetGame(ctx, c)
	if err != nil {
		return err
	}
	if !ok {
		return s.notify(ctx, c, "Ближайших игр нет")
	}

	if existing, booked, err := s.findActiveBooking(ctx, c.user.ID, ev.ID); err != nil {
		return err
	} else if booked {
		return s.alreadyBooked(ctx, c, existing)
	}

	b, err := s.bookings.Create(ctx, booking.CreateInput{
		EventID:        ev.ID,
		PlayerName:     c.user.DisplayName,
		UserID:         &c.user.ID,
		BookedByUserID: c.user.ID,
	})
	switch {
	case errors.Is(err, booking.ErrAlreadyBooked):
		return s.notify(ctx, c, "Вы уже записаны")
	case err != nil:
		return fmt.Errorf("create booking: %w", err)
	}

	if b.Status == booking.StatusWaitlist {
		position, err := s.waitlistPosition(ctx, ev.ID, b.ID)
		if err != nil {
			return err
		}
		if err := s.sendText(ctx, c.peerID, fmt.Sprintf("резерв %d - %s", position, b.PlayerName), ""); err != nil {
			return err
		}
		s.refreshAnnouncementLogged(ctx, c.chat.ID, c.peerID, ev.ID)
		return s.notify(ctx, c, fmt.Sprintf("Мест нет — вы в резерве (%d-й)", position))
	}

	seat := 0
	if b.SeatNo != nil {
		seat = *b.SeatNo
	}
	if err := s.sendText(ctx, c.peerID, fmt.Sprintf("%d - %s", seat, b.PlayerName), ""); err != nil {
		return err
	}
	s.refreshAnnouncementLogged(ctx, c.chat.ID, c.peerID, ev.ID)
	return s.notify(ctx, c, fmt.Sprintf("✅ Вы записаны: место %d", seat))
}

// alreadyBooked answers a repeat "Иду": the lineup keeps its seat, the reserve
// keeps its place in the queue, and the chat stays silent.
func (s *Service) alreadyBooked(ctx context.Context, c *commandCtx, b booking.BookingWithEvent) error {
	switch {
	case b.Status == booking.StatusConfirmed && b.SeatNo != nil:
		return s.notify(ctx, c, fmt.Sprintf("Вы уже записаны: место %d", *b.SeatNo))
	case b.Status == booking.StatusWaitlist:
		position, err := s.waitlistPosition(ctx, b.EventID, b.ID)
		if err != nil {
			return err
		}
		return s.notify(ctx, c, fmt.Sprintf("Вы уже в резерве (%d-й)", position))
	default:
		return s.notify(ctx, c, "Вы уже записаны")
	}
}

// handleSkip is the "Не иду" button: it cancels the caller's booking (the game
// in the payload, otherwise the nearest one where they are booked).
func (s *Service) handleSkip(ctx context.Context, c *commandCtx) error {
	return s.cancelFor(ctx, c)
}

// handleCancel is «отмена» typed by hand: the same cancellation, but the answer
// goes to the chat because a typed command has no button event to answer.
func (s *Service) handleCancel(ctx context.Context, c *commandCtx) error {
	return s.cancelFor(ctx, c)
}

// cancelFor cancels the caller's booking and reports what happened, including
// who took the freed seat: a promotion is never silent (ARCHITECTURE §16).
// A button press gets a personal snackbar; a typed command gets a chat line.
func (s *Service) cancelFor(ctx context.Context, c *commandCtx) error {
	me, booked, err := s.nearestBooking(ctx, c)
	if err != nil {
		return err
	}
	if !booked {
		if c.vkEventID != "" {
			return s.notify(ctx, c, "Вы не записаны")
		}
		return s.sendText(ctx, c.peerID, "Активной записи не нашёл. Посмотреть записи: «мои записи».", "")
	}

	res, err := s.bookings.Cancel(ctx, me.EventID, me.ID)
	if err != nil {
		if errors.Is(err, booking.ErrNotFound) || errors.Is(err, booking.ErrNotActive) {
			if c.vkEventID != "" {
				return s.notify(ctx, c, "Запись уже отменена")
			}
			return s.sendText(ctx, c.peerID, "Запись уже отменена.", "")
		}
		return fmt.Errorf("cancel booking: %w", err)
	}

	line, err := s.cancelLine(ctx, me)
	if err != nil {
		return err
	}
	if res.Promoted != nil {
		seat := 0
		if res.Promoted.SeatNo != nil {
			seat = *res.Promoted.SeatNo
		}
		line += fmt.Sprintf("\n%d - %s из резерва", seat, res.Promoted.PlayerName)
	}
	if err := s.sendText(ctx, c.peerID, line, ""); err != nil {
		return err
	}
	s.refreshAnnouncementLogged(ctx, c.chat.ID, c.peerID, me.EventID)
	return s.notify(ctx, c, "Запись отменена")
}

// cancelLine renders what the chat sees when someone steps out: the seat number
// goes before the name, and the reserve keeps its own numbering.
func (s *Service) cancelLine(ctx context.Context, b booking.BookingWithEvent) (string, error) {
	if b.Status == booking.StatusConfirmed && b.SeatNo != nil {
		return fmt.Sprintf("%d - %s минус", *b.SeatNo, b.PlayerName), nil
	}
	position, err := s.waitlistPosition(ctx, b.EventID, b.ID)
	if err != nil {
		return "", err
	}
	if position > 0 {
		return fmt.Sprintf("резерв %d - %s минус", position, b.PlayerName), nil
	}
	return fmt.Sprintf("%s - пропускает", b.PlayerName), nil
}

// findActiveBooking returns the caller's active booking for the event in the
// payload. Without an event id a single active booking is used.
func (s *Service) findActiveBooking(ctx context.Context, userID, eventID int64) (booking.BookingWithEvent, bool, error) {
	mine, err := s.bookings.ListActiveByUser(ctx, userID, s.now().UTC())
	if err != nil {
		return booking.BookingWithEvent{}, false, fmt.Errorf("list user bookings: %w", err)
	}
	if eventID == 0 {
		if len(mine) == 1 {
			return mine[0], true, nil
		}
		return booking.BookingWithEvent{}, false, nil
	}
	for _, b := range mine {
		if b.EventID == eventID {
			return b, true, nil
		}
	}
	return booking.BookingWithEvent{}, false, nil
}

// waitlistPosition returns the 1-based place in the reserve, or 0 when the
// booking is not in the reserve.
func (s *Service) waitlistPosition(ctx context.Context, eventID, bookingID int64) (int, error) {
	queued, err := s.bookings.ListByEvent(ctx, eventID, booking.StatusWaitlist)
	if err != nil {
		return 0, fmt.Errorf("list reserve: %w", err)
	}
	for i, b := range queued {
		if b.ID == bookingID {
			return i + 1, nil
		}
	}
	return 0, nil
}

// nearestBooking finds the booking "Не иду" refers to: the payload's game, or
// otherwise the nearest one where the caller is booked.
func (s *Service) nearestBooking(ctx context.Context, c *commandCtx) (booking.BookingWithEvent, bool, error) {
	if c.eventID != 0 {
		return s.findActiveBooking(ctx, c.user.ID, c.eventID)
	}

	mine, err := s.bookings.ListActiveByUser(ctx, c.user.ID, s.now().UTC())
	if err != nil {
		return booking.BookingWithEvent{}, false, fmt.Errorf("list user bookings: %w", err)
	}
	if len(mine) == 0 {
		return booking.BookingWithEvent{}, false, nil
	}
	return mine[0], true, nil // the store orders bookings by game time
}

// targetGame is the game an unqualified press refers to: the payload's game, or
// the nearest upcoming game of the chat — the persistent keyboard has no game
// of its own, and a chat plays one game at a time in practice.
func (s *Service) targetGame(ctx context.Context, c *commandCtx) (event.Event, bool, error) {
	if c.eventID != 0 {
		ev, err := s.events.Get(ctx, c.chat.ID, c.eventID)
		if errors.Is(err, event.ErrNotFound) {
			return event.Event{}, false, nil
		}
		if err != nil {
			return event.Event{}, false, fmt.Errorf("get event: %w", err)
		}
		return ev, true, nil
	}

	games, err := s.events.ListUpcomingWithCounts(ctx, c.chat.ID, s.now().UTC(), 1)
	if err != nil {
		return event.Event{}, false, fmt.Errorf("list games: %w", err)
	}
	if len(games) == 0 {
		return event.Event{}, false, nil
	}
	return games[0].Event, true, nil
}

// notify answers a button press with a snackbar: a short message only the
// presser sees, so the rest of the chat does not learn who pressed what. A
// typed command has no button event to answer, and there the chat line is it.
func (s *Service) notify(ctx context.Context, c *commandCtx, text string) error {
	if c.vkEventID == "" {
		return nil
	}
	data := fmt.Sprintf(`{"type":"show_snackbar","text":%q}`, text)
	if err := s.messenger.AnswerMessageEvent(ctx, c.vkUserID, c.peerID, c.vkEventID, data); err != nil {
		return fmt.Errorf("snackbar: %w", err)
	}
	c.answered = true
	return nil
}

// handleMyBookings lists the caller's active bookings with a cancel button
// per booking.
func (s *Service) handleMyBookings(ctx context.Context, c *commandCtx) error {
	mine, err := s.bookings.ListActiveByUser(ctx, c.user.ID, s.now().UTC())
	if err != nil {
		return fmt.Errorf("list user bookings: %w", err)
	}
	if len(mine) == 0 {
		return s.sendText(ctx, c.peerID, "У вас нет активных записей. Ближайшие игры: «игры».", "")
	}

	var b strings.Builder
	b.WriteString("Ваши записи:\n")
	rows := make([][]Button, 0, len(mine))
	for _, m := range mine {
		status := "в составе"
		if m.Status == booking.StatusWaitlist {
			status = "в резерве"
		}
		fmt.Fprintf(&b, "\n• %s — %s (%s)\n", m.EventTitle, s.formatWhenShort(m.EventStartsAt), status)
		rows = append(rows, []Button{
			TextButton("Отменить: "+m.EventTitle, EventCommandPayload(cmdCancelBooking, m.EventID), ColorNegative),
		})
	}
	kb, err := (Keyboard{Inline: true, Buttons: rows}).Marshal()
	if err != nil {
		return err
	}
	return s.sendText(ctx, c.peerID, b.String(), kb)
}

// refreshAnnouncement rewrites the game announcement with the current roster.
// A game never announced in this chat has no reference, and then there is
// nothing to update: the reply that triggered the call already told the user
// what changed. The same goes for an announcement whose id the bot does not
// know yet (VK answers messages.send with 0 in a conversation): the id arrives
// with the first press on the announcement's own button.
func (s *Service) refreshAnnouncement(ctx context.Context, chatID, peerID, eventID int64) error {
	ref, ok, err := s.announces.Get(ctx, eventID, chat.PlatformVK)
	if err != nil {
		return fmt.Errorf("get announcement: %w", err)
	}
	if !ok || ref.MessageID == 0 {
		return nil
	}

	ev, err := s.events.Get(ctx, chatID, eventID)
	if err != nil {
		return fmt.Errorf("get event: %w", err)
	}

	confirmed, waitlist, err := s.roster(ctx, eventID)
	if err != nil {
		return err
	}

	text, kb, err := s.renderAnnouncement(ev, confirmed, waitlist)
	if err != nil {
		return err
	}
	if _, err := s.messenger.EditMessage(ctx, peerID, ref.MessageID, text, kb); err != nil {
		return fmt.Errorf("edit announcement: %w", err)
	}
	return nil
}

// refreshAnnouncementLogged updates the announcement and only writes a failure
// to the log: the booking itself already happened and was reported, so a stale
// announcement must not turn into an error for the person who pressed the
// button.
func (s *Service) refreshAnnouncementLogged(ctx context.Context, chatID, peerID, eventID int64) {
	if err := s.refreshAnnouncement(ctx, chatID, peerID, eventID); err != nil {
		s.log.Printf("vk: refresh announcement of game %d: %v", eventID, err)
	}
}

// gameDraft is what "создать игру" produces: tomorrow at 19:00, 12 мест,
// «Волейбол» — adjusted by whatever the administrator typed. dateSet, timeSet
// and weekday say what was actually typed, so the caller can fall back to the
// chat schedule for the rest.
type gameDraft struct {
	startsAt time.Time
	title    string
	location string
	capacity int

	dateSet bool
	timeSet bool
	// weekday is the weekday named in the text, or -1 when there is none.
	weekday int
}

var (
	// timeRE matches "19:00" (and "19.00").
	timeRE = regexp.MustCompile(`\b([01]?\d|2[0-3])[:.](\d{2})\b`)
	// dateRE matches "27.09" and "27.09.2026".
	dateRE = regexp.MustCompile(`\b(\d{1,2})\.(\d{1,2})(?:\.(\d{4}))?\b`)
	// capacityRE matches a lone number: "12" or "12 мест".
	capacityRE = regexp.MustCompile(`\b(\d{1,3})\b`)
)

// commandWords carry no title in "создать игру …" and are dropped from it.
var commandWords = []string{
	"создать", "создай", "создание", "новую", "новый", "игру", "игра", "игры",
	"мест", "места", "человек", "чел", "на", "в", "и",
}

// parseCreateGame reads "создать игру [ДД.ММ[.ГГГГ]] [ЧЧ:ММ] [N мест]".
// Anything left after the recognised parts becomes the title, and a missing
// part falls back to a default — "создать игру" alone already creates a game.
//
// The bare number rule means a title with a number in it ("зал 3") is read as
// the capacity: the documented way to pass it is "… 12 мест".
func parseCreateGame(text string, now time.Time, loc *time.Location) gameDraft {
	draft := gameDraft{title: defaultTitle, capacity: defaultCapacity, weekday: -1}

	rest := strings.ToLower(text)
	day := now.In(loc).AddDate(0, 0, 1)
	hour, minute := defaultHour, 0

	// День недели словами («создать игру в среду 19:00»): дату посчитает
	// gameStart, здесь он только снимается с текста, чтобы не попасть в
	// название игры.
	if weekday, word, ok := firstWeekdayWord(rest); ok {
		draft.weekday = weekday
		rest = strings.Replace(rest, word, " ", 1)
	}

	if m := timeRE.FindStringSubmatch(rest); m != nil {
		hour, _ = strconv.Atoi(m[1])
		minute, _ = strconv.Atoi(m[2])
		draft.timeSet = true
		rest = strings.Replace(rest, m[0], " ", 1)
	}
	if m := dateRE.FindStringSubmatch(rest); m != nil {
		dayNum, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		year := day.Year()
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
		}
		if dayNum >= 1 && dayNum <= 31 && month >= 1 && month <= 12 {
			day = time.Date(year, time.Month(month), dayNum, 0, 0, 0, 0, loc)
			draft.dateSet = true
		}
		rest = strings.Replace(rest, m[0], " ", 1)
	}
	if m := capacityRE.FindStringSubmatch(rest); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			draft.capacity = n
		}
		rest = strings.Replace(rest, m[0], " ", 1)
	}

	draft.startsAt = time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc).UTC()
	if title := titleOf(rest); title != "" {
		draft.title = title
	}
	return draft
}

// firstWeekdayWord finds a weekday named in the text and returns it together
// with the word itself, so the caller can take that word out of the title:
// «создать игру в среду 19:00» is not a game called «в среду».
func firstWeekdayWord(text string) (weekday int, word string, ok bool) {
	for _, field := range strings.Fields(text) {
		if day, isDay := schedule.ParseWeekday(field); isDay {
			return day, field, true
		}
	}
	return 0, "", false
}

// titleOf keeps the words a human typed as the game name and drops the
// command words around them.
func titleOf(rest string) string {
	fields := strings.Fields(rest)
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if slices.Contains(commandWords, f) {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}

// handleSettings shows the schedule screen of the chat. When it was opened by a
// button, the screen rewrites the very message the button was pressed on (VK
// tells us its id), so the chat keeps one live settings message instead of a
// pile of them; a typed command posts a new one.
func (s *Service) handleSettings(ctx context.Context, c *commandCtx) error {
	ok, err := s.requireAdmin(ctx, c, settingsRefusal)
	if err != nil || !ok {
		return err
	}
	return s.refreshSettings(ctx, c, "")
}

// handleSlotAdd records one weekday of the schedule. The weekday and the time
// come either from a typed message («вс 10:00») or from the settings buttons;
// without them the bot explains the expected format.
func (s *Service) handleSlotAdd(ctx context.Context, c *commandCtx) error {
	ok, err := s.requireAdmin(ctx, c, settingsRefusal)
	if err != nil || !ok {
		return err
	}
	if !c.hasSlot {
		return s.sendText(ctx, c.peerID, "Пришлите день и время, например: «вс 10:00».", "")
	}

	slot, err := s.schedule.Set(ctx, c.chat.ID, c.weekday, c.minutes)
	if err != nil {
		return fmt.Errorf("set schedule slot: %w", err)
	}
	return s.refreshSettings(ctx, c,
		fmt.Sprintf("Записано: %s в %s", schedule.LongWeekday(slot.Weekday), slot.At()))
}

// handleSlotRemove drops a weekday from the schedule.
func (s *Service) handleSlotRemove(ctx context.Context, c *commandCtx) error {
	ok, err := s.requireAdmin(ctx, c, settingsRefusal)
	if err != nil || !ok {
		return err
	}

	removed, err := s.schedule.Delete(ctx, c.chat.ID, c.weekday)
	if err != nil {
		return fmt.Errorf("delete schedule slot: %w", err)
	}
	note := fmt.Sprintf("Убрал: %s", schedule.LongWeekday(c.weekday))
	if !removed {
		note = fmt.Sprintf("%s в расписании и не было", schedule.LongWeekday(c.weekday))
	}
	return s.refreshSettings(ctx, c, note)
}

// requireAdmin sends the refusal to the chat and reports whether the caller
// may act. Rights are read from chat_admins; the VK API is asked only at
// connect time.
func (s *Service) requireAdmin(ctx context.Context, c *commandCtx, refusal string) (bool, error) {
	admin, err := s.chats.IsChatAdmin(ctx, c.chat.ID, c.user.ID)
	if err != nil {
		return false, fmt.Errorf("check chat admin: %w", err)
	}
	if admin {
		return true, nil
	}
	if err := s.sendText(ctx, c.peerID, refusal, ""); err != nil {
		return false, err
	}
	return false, nil
}

// refreshSettings rewrites the settings screen after a change: the very
// message the callback button was pressed on when VK told us its id, and a new
// message otherwise (a typed command has no message to rewrite).
func (s *Service) refreshSettings(ctx context.Context, c *commandCtx, note string) error {
	text, kb, err := s.renderSettings(ctx, c.chat)
	if err != nil {
		return err
	}
	if note != "" {
		text = note + "\n\n" + text
	}

	if c.messageID != 0 {
		if _, err := s.messenger.EditMessage(ctx, c.peerID, c.messageID, text, kb); err != nil {
			return fmt.Errorf("edit settings: %w", err)
		}
		return nil
	}
	return s.sendText(ctx, c.peerID, text, kb)
}

// renderSettings renders the schedule screen: what is set now, one button per
// configured weekday to drop it, and a hint about the input format.
func (s *Service) renderSettings(ctx context.Context, ch chat.Chat) (string, string, error) {
	slots, err := s.schedule.List(ctx, ch.ID)
	if err != nil {
		return "", "", fmt.Errorf("list schedule: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Расписание игр чата «%s»\n", ch.Title)
	fmt.Fprintf(&b, "Игра: %s, %d мест\n\n", defaultTitle, defaultCapacity)
	fmt.Fprintf(&b, "Сейчас: %s\n\n", schedule.Describe(slots))
	b.WriteString("Время указывается в поясе чата. Чтобы задать или изменить день, пришлите «/настройки вс 10:00», убрать — «/настройки убрать сб».")

	rows := make([][]Button, 0, len(slots)+1)
	for _, slot := range slots {
		label := fmt.Sprintf("%s %s ✕", schedule.LongWeekday(slot.Weekday), slot.At())
		rows = append(rows, []Button{
			CallbackButton(label, SlotCommandPayload(cmdSlotRemove, slot.Weekday), ColorNegative),
		})
	}
	rows = append(rows, []Button{
		CallbackButton("Добавить день и время", CommandPayload(cmdSlotAdd), ColorSecondary),
	})

	kb, err := Keyboard{Inline: true, Buttons: rows}.Marshal()
	if err != nil {
		return "", "", err
	}
	return b.String(), kb, nil
}

// sendText replies in the chat. A caller without a keyboard of its own gets
// the persistent keyboard: the one that stays under the input field, so the
// buttons never scroll out of reach in a busy chat.
func (s *Service) sendText(ctx context.Context, peerID int64, text, keyboard string) error {
	if keyboard == "" {
		kb, err := persistentKeyboard()
		if err != nil {
			return err
		}
		keyboard = kb
	}
	if _, err := s.messenger.SendMessage(ctx, peerID, text, keyboard); err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	return nil
}

func (s *Service) sendWelcome(ctx context.Context, peerID int64, displayName, chatTitle string) error {
	kb, err := persistentKeyboard()
	if err != nil {
		return err
	}
	text := fmt.Sprintf(
		"Привет, %s!\nЭто бот записи на игры чата «%s».\n\n"+
			"Команды:\n"+
			"/игры — ближайшие игры и запись\n"+
			"/мои — ваши записи\n"+
			"/отмена — отменить свою запись\n"+
			"/старт — создать игру и анонс (администратор)\n"+
			"/настройки — расписание: /настройки вс 10:00 (администратор)\n"+
			"/помощь — эта справка\n\n"+
			"Кнопки «Иду» и «Не иду» под полем ввода — для записи. Бот отвечает только "+
			"на команды и кнопки, поэтому не мешает вашей переписке.",
		displayName, chatTitle)
	return s.sendText(ctx, peerID, text, kb)
}

// persistentKeyboard is the keyboard that stays under the chat input instead
// of scrolling away with a message (VK: no "inline", no one_time). It carries
// the two actions of the sign-up flow; the rest is admin slash commands and
// the buttons of the pinned announcement. Buttons are callback buttons: VK
// delivers a press as message_event, so pressing one posts nothing to the chat
// by itself. Verified against the live community.
func persistentKeyboard() (string, error) {
	kb := Keyboard{
		Buttons: [][]Button{
			{
				CallbackButton("Иду", CommandPayload(cmdAttend), ColorPositive),
				CallbackButton("Не иду", CommandPayload(cmdSkip), ColorNegative),
			},
		},
	}
	return kb.Marshal()
}

// parseCommand maps a message to a command with its parameters. A button
// payload always wins: buttons are explicit. Otherwise only slash commands
// are commands — in a live chat people talk to each other, and everything
// without a slash is conversation the bot must not answer.
//
// Two deliberate exceptions, both narrow:
//   - a chat that is not connected yet has no keyboard at all, so there
//     "подключить" (or /connect) is accepted without a slash: it is a
//     one-time action;
//   - the schedule screen shows the exact format it expects, and /slot
//     carries the weekday and the time: "/slot вс 10:00".
func (s *Service) parseCommand(text, payload string, connected bool) parsedCommand {
	if p := ParseButtonPayload(payload); p.Command != "" {
		return parsedCommand{cmd: p.Command, eventID: p.EventID, weekday: p.Weekday}
	}

	t := strings.ToLower(strings.TrimSpace(text))

	if !connected {
		if t == "подключить" || t == "/connect" {
			return parsedCommand{cmd: cmdConnect}
		}
		return parsedCommand{}
	}

	name, args, ok := slashCommand(t)
	if !ok {
		return parsedCommand{}
	}

	switch name {
	case "games", "игры":
		return parsedCommand{cmd: cmdGames}
	case "my", "мои", "моизаписи":
		return parsedCommand{cmd: cmdMyBookings}
	case "cancel", "отмена":
		return parsedCommand{cmd: cmdCancelBooking}
	case "create", "создать", "старт":
		return parsedCommand{cmd: cmdCreateGame}
	case "settings", "настройки":
		// /настройки вс 10:00 — сразу задать день, без аргументов — экран.
		if slot, ok := parseSlotMessage(args); ok {
			return slot
		}
		return parsedCommand{cmd: cmdSettings}
	case "slot", "слот":
		// /slot вс 10:00 — задать день, /slot убрать сб — убрать его.
		// Без аргументов команда отвечает подсказкой о формате.
		if slot, ok := parseSlotMessage(args); ok {
			return slot
		}
		return parsedCommand{cmd: cmdSlotAdd}
	case "connect", "подключить":
		return parsedCommand{cmd: cmdConnect}
	case "help", "помощь":
		return parsedCommand{cmd: cmdStart}
	}
	return parsedCommand{}
}

// slashCommand splits "/create 27.09 19:00" into "create" and "27.09 19:00".
// ok is false for anything without a leading slash: that is conversation, not
// a command.
func slashCommand(text string) (name, args string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	name, args, _ = strings.Cut(strings.TrimPrefix(text, "/"), " ")
	return strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(args), true
}

// stripCommandWord removes a leading "/games" token, so the rest of the
// message is parsed as arguments: "/create 27.09 19:00" → "27.09 19:00".
func stripCommandWord(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return text
	}
	return strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
}

// slotFillers are the words a schedule message may contain besides the
// weekday and the time: «добавить в среду 19:00».
var slotFillers = map[string]bool{
	"добавить": true, "добавь": true, "поставить": true, "поставь": true, "задай": true,
	"в": true, "на": true, "по": true, "время": true,
	"игру": true, "игра": true, "игры": true, "расписание": true, "расписания": true,
}

// parseSlotMessage reads a message that talks about the schedule only:
// «вс 10:00» adds a weekday, «убрать сб» removes one. Anything else (an
// unknown word) is left to the other commands.
func parseSlotMessage(text string) (parsedCommand, bool) {
	weekday, minutes := 0, 0
	hasWeekday, hasTime, remove := false, false, false

	for _, word := range strings.Fields(text) {
		word = strings.Trim(word, ".,")
		switch {
		case word == "" || slotFillers[word]:
			continue
		case weekdayWordsForRemove[word]:
			remove = true
			continue
		}
		if day, ok := schedule.ParseWeekday(word); ok {
			weekday, hasWeekday = day, true
			continue
		}
		if at, ok := schedule.ParseTime(word); ok {
			minutes, hasTime = at, true
			continue
		}
		return parsedCommand{}, false
	}

	switch {
	case hasWeekday && hasTime:
		return parsedCommand{cmd: cmdSlotAdd, weekday: weekday, minutes: minutes, hasSlot: true}, true
	case hasWeekday && remove:
		return parsedCommand{cmd: cmdSlotRemove, weekday: weekday}, true
	}
	return parsedCommand{}, false
}

// weekdayWordsForRemove are the verbs that turn a slot message into a
// removal.
var weekdayWordsForRemove = map[string]bool{
	"убрать": true, "убери": true, "удалить": true, "удали": true, "снять": true, "сними": true,
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
