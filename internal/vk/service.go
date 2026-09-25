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
	// cmdBook signs the pressed user up for one game (payload carries the id).
	cmdBook = "book"
	// cmdSkip is the "Пропускаю" button: it cancels the user's booking.
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
	parsed := s.parseCommand(msg.Text, msg.Payload)

	// An unconnected conversation is not a Chat yet: the connection
	// request is the only command that can act on it.
	if ch == nil {
		if parsed.cmd == cmdConnect {
			return s.handleConnect(ctx, msg)
		}
		s.log.Printf("vk: message from unconnected peer %d", msg.PeerID)
		return s.sendText(ctx, msg.PeerID, "Эта беседа ещё не подключена. Напишите «подключить», чтобы подключить её.", "")
	}

	user, err := s.ensureUser(ctx, msg.FromID)
	if err != nil {
		return err
	}
	return s.handleCommand(ctx, commandCtx{
		peerID:  msg.PeerID,
		chat:    *ch,
		user:    user,
		cmd:     parsed.cmd,
		eventID: parsed.eventID,
		weekday: parsed.weekday,
		minutes: parsed.minutes,
		hasSlot: parsed.hasSlot,
		text:    msg.Text,
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
func (s *Service) handleCommand(ctx context.Context, c commandCtx) error {
	switch c.cmd {
	case cmdStart:
		return s.sendWelcome(ctx, c.peerID, c.user.DisplayName, c.chat.Title)
	case cmdConnect:
		return s.sendText(ctx, c.peerID, fmt.Sprintf("Беседа «%s» уже подключена.", c.chat.Title), "")
	case cmdGames:
		return s.handleGames(ctx, c)
	case cmdCreateGame:
		return s.handleCreateGame(ctx, c)
	case cmdBook:
		return s.handleBook(ctx, c)
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
		return s.sendText(ctx, c.peerID, "Не понял команду. Напишите «игры» или /start.", "")
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

	parsed := ParseButtonPayload(ev.Payload)
	return s.handleCommand(ctx, commandCtx{
		peerID:    ev.PeerID,
		chat:      *ch,
		user:      user,
		cmd:       parsed.Command,
		eventID:   parsed.EventID,
		weekday:   parsed.Weekday,
		messageID: ev.ConversationMessageID,
	})
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

// handleGames lists the upcoming games of the chat with their free slots and
// a button per game: signing up is one press from here.
func (s *Service) handleGames(ctx context.Context, c commandCtx) error {
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
		fmt.Fprintf(&b, "\n%d) %s — %s\n", i+1, g.Title, s.formatWhen(g.StartsAt))
		fmt.Fprintf(&b, "   свободно %d из %d", g.Free, g.Capacity)
		if g.Waitlist > 0 {
			fmt.Fprintf(&b, ", в резерве %d", g.Waitlist)
		}
		b.WriteString("\n")
		rows = append(rows, []Button{
			TextButton("Записаться: "+g.Title, EventCommandPayload(cmdBook, g.ID), ColorPrimary),
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
func (s *Service) handleCreateGame(ctx context.Context, c commandCtx) error {
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
	// not announce the same game twice.
	if existing, ok, err := s.findGameAt(ctx, c.chat.ID, startsAt); err != nil {
		return err
	} else if ok {
		return s.sendText(ctx, c.peerID, fmt.Sprintf(
			"Игра на %s уже создана — записывайтесь по анонсу выше.", s.formatWhen(existing.StartsAt)), "")
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

	// The announcement is the message everyone signs up under: remember its
	// id so later changes rewrite this message instead of posting a new one.
	text, kb, err := s.renderAnnouncement(ev, nil, nil)
	if err != nil {
		return err
	}
	msgID, err := s.messenger.SendMessage(ctx, c.peerID, text, kb)
	if err != nil {
		return fmt.Errorf("send announcement: %w", err)
	}
	if err := s.announces.Save(ctx, announce.Ref{
		EventID:        ev.ID,
		Platform:       chat.PlatformVK,
		ExternalChatID: strconv.FormatInt(c.peerID, 10),
		MessageID:      msgID,
	}); err != nil {
		return fmt.Errorf("save announcement: %w", err)
	}
	return nil
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

// renderAnnouncement renders a game message: who is in the lineup, who is in
// the reserve, and the buttons to sign up or skip. Once the last slot is
// taken the button becomes "В резерв" — the same booking call, because the
// store decides between confirmed and waitlist.
func (s *Service) renderAnnouncement(ev event.Event, confirmed, waitlist []booking.Booking) (string, string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", ev.Title, s.formatWhen(ev.StartsAt))
	if strings.TrimSpace(ev.Location) != "" {
		fmt.Fprintf(&b, "Место: %s\n", ev.Location)
	}
	fmt.Fprintf(&b, "\nСостав (%d/%d):\n", len(confirmed), ev.Capacity)
	if len(confirmed) == 0 {
		b.WriteString("— пока никого\n")
	}
	for i, bk := range confirmed {
		fmt.Fprintf(&b, "%d. %s\n", i+1, bk.PlayerName)
	}
	if len(waitlist) > 0 {
		fmt.Fprintf(&b, "\nРезерв (%d):\n", len(waitlist))
		for i, bk := range waitlist {
			fmt.Fprintf(&b, "%d. %s\n", i+1, bk.PlayerName)
		}
	}

	label := "Записаться"
	if len(confirmed) >= ev.Capacity {
		label = "В резерв"
	}
	kb := Keyboard{Inline: true, Buttons: [][]Button{
		{TextButton(label, EventCommandPayload(cmdBook, ev.ID), ColorPrimary)},
		{TextButton("Пропускаю", EventCommandPayload(cmdSkip, ev.ID), ColorSecondary)},
	}}
	raw, err := kb.Marshal()
	if err != nil {
		return "", "", err
	}
	return b.String(), raw, nil
}

// formatWhen renders a game time the way a chat reads it: "сегодня 19:00",
// "завтра 19:00" or "27.09 в 19:00" for anything further out.
func (s *Service) formatWhen(t time.Time) string {
	local := t.In(s.loc)
	today := s.now().In(s.loc)
	day := local.Format("02.01")
	switch {
	case sameDay(local, today):
		day = "сегодня"
	case sameDay(local, today.AddDate(0, 0, 1)):
		day = "завтра"
	}
	return fmt.Sprintf("%s в %s", day, local.Format("15:04"))
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// handleBook signs the user up for one game. booking.Create decides between
// the lineup and the reserve, so a full game puts the user in the reserve.
func (s *Service) handleBook(ctx context.Context, c commandCtx) error {
	if c.eventID == 0 {
		return s.sendText(ctx, c.peerID, "Выберите игру: напишите «игры» и нажмите «Записаться» под нужной игрой.", "")
	}
	ev, err := s.events.Get(ctx, c.chat.ID, c.eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return s.sendText(ctx, c.peerID, "Такой игры не нашёл — возможно, она уже прошла.", "")
		}
		return fmt.Errorf("get event: %w", err)
	}

	b, err := s.bookings.Create(ctx, booking.CreateInput{
		EventID:        ev.ID,
		PlayerName:     c.user.DisplayName,
		UserID:         &c.user.ID,
		BookedByUserID: c.user.ID,
	})
	switch {
	case errors.Is(err, booking.ErrAlreadyBooked):
		return s.sendText(ctx, c.peerID, "Вы уже записаны на эту игру.", "")
	case err != nil:
		return fmt.Errorf("create booking: %w", err)
	}

	reply := fmt.Sprintf("Вы записаны: «%s» — %s.", ev.Title, s.formatWhen(ev.StartsAt))
	if b.Status == booking.StatusWaitlist {
		reply = fmt.Sprintf("Мест нет — вы в резерве: «%s» — %s.", ev.Title, s.formatWhen(ev.StartsAt))
	}
	if err := s.sendText(ctx, c.peerID, reply, ""); err != nil {
		return err
	}
	return s.refreshAnnouncement(ctx, c.chat.ID, c.peerID, ev.ID)
}

// handleSkip is the "Пропускаю" button: it cancels the user's booking for the
// game in the payload.
func (s *Service) handleSkip(ctx context.Context, c commandCtx) error {
	return s.cancelBooking(ctx, c, "Вы не записаны на эту игру.")
}

// handleCancel is «отмена» typed by hand: it works when the user has exactly
// one active booking, otherwise the buttons of «мои записи» are the way.
func (s *Service) handleCancel(ctx context.Context, c commandCtx) error {
	return s.cancelBooking(ctx, c, "Активной записи не нашёл. Посмотреть записи: «мои записи».")
}

// cancelBooking cancels the caller's booking and reports what happened,
// including who took the freed slot: a promotion is never silent
// (ARCHITECTURE §16).
func (s *Service) cancelBooking(ctx context.Context, c commandCtx, notFound string) error {
	b, ok, err := s.findActiveBooking(ctx, c.user.ID, c.eventID)
	if err != nil {
		return err
	}
	if !ok {
		return s.sendText(ctx, c.peerID, notFound, "")
	}

	res, err := s.bookings.Cancel(ctx, b.EventID, b.ID)
	if err != nil {
		if errors.Is(err, booking.ErrNotFound) || errors.Is(err, booking.ErrNotActive) {
			return s.sendText(ctx, c.peerID, "Запись уже отменена.", "")
		}
		return fmt.Errorf("cancel booking: %w", err)
	}

	reply := fmt.Sprintf("Запись на «%s» отменена.", b.EventTitle)
	if res.Promoted != nil {
		reply += fmt.Sprintf("\nМесто занял %s из резерва.", res.Promoted.PlayerName)
	}
	if err := s.sendText(ctx, c.peerID, reply, ""); err != nil {
		return err
	}
	return s.refreshAnnouncement(ctx, c.chat.ID, c.peerID, b.EventID)
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

// handleMyBookings lists the caller's active bookings with a cancel button
// per booking.
func (s *Service) handleMyBookings(ctx context.Context, c commandCtx) error {
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
		fmt.Fprintf(&b, "\n• %s — %s (%s)\n", m.EventTitle, s.formatWhen(m.EventStartsAt), status)
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
// what changed.
func (s *Service) refreshAnnouncement(ctx context.Context, chatID, peerID, eventID int64) error {
	ref, ok, err := s.announces.Get(ctx, eventID, chat.PlatformVK)
	if err != nil {
		return fmt.Errorf("get announcement: %w", err)
	}
	if !ok {
		return nil
	}

	ev, err := s.events.Get(ctx, chatID, eventID)
	if err != nil {
		return fmt.Errorf("get event: %w", err)
	}

	all, err := s.bookings.ListByEvent(ctx, eventID, booking.StatusConfirmed, booking.StatusWaitlist)
	if err != nil {
		return fmt.Errorf("list bookings: %w", err)
	}
	var confirmed, waitlist []booking.Booking
	for _, b := range all {
		if b.Status == booking.StatusConfirmed {
			confirmed = append(confirmed, b)
		} else {
			waitlist = append(waitlist, b)
		}
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

// handleSettings shows the schedule screen of the chat.
func (s *Service) handleSettings(ctx context.Context, c commandCtx) error {
	ok, err := s.requireAdmin(ctx, c, settingsRefusal)
	if err != nil || !ok {
		return err
	}
	text, kb, err := s.renderSettings(ctx, c.chat)
	if err != nil {
		return err
	}
	return s.sendText(ctx, c.peerID, text, kb)
}

// handleSlotAdd records one weekday of the schedule. The weekday and the time
// come either from a typed message («вс 10:00») or from the settings buttons;
// without them the bot explains the expected format.
func (s *Service) handleSlotAdd(ctx context.Context, c commandCtx) error {
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
func (s *Service) handleSlotRemove(ctx context.Context, c commandCtx) error {
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
func (s *Service) requireAdmin(ctx context.Context, c commandCtx, refusal string) (bool, error) {
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
func (s *Service) refreshSettings(ctx context.Context, c commandCtx, note string) error {
	text, kb, err := s.renderSettings(ctx, c.chat)
	if err != nil {
		return err
	}
	text = note + "\n\n" + text

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
	b.WriteString("Время указывается в поясе чата. Чтобы добавить или изменить день, пришлите сообщение вида «вс 10:00».")

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
	text := fmt.Sprintf(
		"Привет, %s!\nЭто бот записи на игры чата «%s».\n\n"+
			"Команды:\n"+
			"• «игры» — ближайшие игры и запись\n"+
			"• «мои записи» — ваши записи\n"+
			"• «отмена» — отменить свою запись\n"+
			"• «создать игру» — новая игра и анонс в беседу (дата берётся из расписания)\n"+
			"• «настройки» — расписание игр: дни недели и время (только администратор)",
		displayName, chatTitle)
	return s.sendText(ctx, peerID, text, kb)
}

// defaultKeyboard is the always-available set of buttons. Admin-only actions
// are offered to everyone: a member who is not an administrator gets a clear
// refusal instead of a hidden feature.
func defaultKeyboard() (string, error) {
	kb := Keyboard{
		Inline: true,
		Buttons: [][]Button{
			{
				TextButton("Игры", CommandPayload(cmdGames), ColorPrimary),
				TextButton("Мои записи", CommandPayload(cmdMyBookings), ColorSecondary),
			},
			{
				TextButton("Создать игру", CommandPayload(cmdCreateGame), ColorPositive),
				TextButton("Настройки", CommandPayload(cmdSettings), ColorSecondary),
			},
		},
	}
	return kb.Marshal()
}

// parseCommand maps a message text and/or a button payload to a command with
// its parameters. A payload always wins: it carries the button's intent
// together with what it applies to. Text is checked from the most specific
// phrase down, and a message that is *only* a schedule slot («вс 10:00»,
// «убрать сб») is recognised last, so «создать игру в среду 19:00» stays a
// game and not a schedule change.
func (s *Service) parseCommand(text, payload string) parsedCommand {
	if p := ParseButtonPayload(payload); p.Command != "" {
		return parsedCommand{cmd: p.Command, eventID: p.EventID, weekday: p.Weekday}
	}

	t := strings.ToLower(strings.TrimSpace(text))
	switch {
	case t == "/start" || t == "start" || t == "начать":
		return parsedCommand{cmd: cmdStart}
	case strings.Contains(t, "подключ"):
		return parsedCommand{cmd: cmdConnect}
	case strings.Contains(t, "настройк") || strings.Contains(t, "расписани"):
		return parsedCommand{cmd: cmdSettings}
	case strings.Contains(t, "созда"):
		return parsedCommand{cmd: cmdCreateGame}
	case strings.Contains(t, "мои запис"):
		return parsedCommand{cmd: cmdMyBookings}
	case strings.Contains(t, "пропуск"):
		return parsedCommand{cmd: cmdSkip}
	case strings.Contains(t, "отмен") || strings.Contains(t, "отпис"):
		return parsedCommand{cmd: cmdCancelBooking}
	case strings.Contains(t, "резерв"):
		return parsedCommand{cmd: cmdBook}
	case strings.Contains(t, "записат") || strings.Contains(t, "запиши"):
		return parsedCommand{cmd: cmdBook}
	case strings.Contains(t, "игр"):
		return parsedCommand{cmd: cmdGames}
	}

	if slot, ok := parseSlotMessage(t); ok {
		return slot
	}
	return parsedCommand{}
}

// slotFillers are the words a schedule message may contain besides the
// weekday and the time: «добавить в среду 19:00», «убрать сб».
var slotFillers = map[string]bool{
	"добавить": true, "добавь": true, "поставить": true, "поставь": true,
	"убрать": true, "убери": true, "удалить": true, "удали": true,
	"снять": true, "сними": true, "задай": true,
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
