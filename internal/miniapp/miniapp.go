// Package miniapp serves the SportEvents mini app inside VK.
//
// Two pages live here:
//
//	<Base>/       — the interface people use from a chat: the games of the
//	                conversation, who is in the lineup (with avatars), sign up
//	                and step out;
//	<Base>/debug/ — the diagnostic page: what VK put into the launch address,
//	                what VK Bridge answered, and the raw log.
//
// The interface reads and writes the same domain services as the bot, so the
// chat and the app cannot disagree about who is signed up: a booking made in
// the app posts the same «N - Имя» line and rewrites the same announcement.
package miniapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"sportevents.local/internal/booking"
	"sportevents.local/internal/chat"
	"sportevents.local/internal/event"
	"sportevents.local/internal/schedule"
)

// assets holds the pages and the VK Bridge bundle. The bundle is kept in the
// repository (8 KB) so the pages do not depend on a CDN inside the WebView.
//
//go:embed app.html debug.html static
var assets embed.FS

// conversationPeerIDMin is the lowest peer_id of a VK group conversation:
// vk_chat_id is the chat number, and the peer id is that number with this
// prefix. The value matches conversationPeerIDMin in the VK adapter.
const conversationPeerIDMin = 2_000_000_000

// gamesLimit caps the list of games on the screen: a phone shows a handful of
// them, not the whole season.
const gamesLimit = 10

// defaultPlayerName is what a booking is recorded under when VK does not name
// the person: better a generic lineup entry than a failed sign-up.
const defaultPlayerName = "Участник"

// The defaults the create form is pre-filled with: the same ones the bot uses
// for «/старт» without arguments.
const (
	defaultTitle    = "Волейбол"
	defaultCapacity = 12
	defaultHour     = 19
)

// defaultLocation is Moscow time: V1 assumes one city per chat (ARCHITECTURE
// §7), and a fixed offset keeps the app working without tzdata. The bot uses
// the same zone, so both tell the same time.
var defaultLocation = time.FixedZone("MSK", 3*60*60)

// ChatStore resolves the conversation the app was launched from and its
// administrators.
type ChatStore interface {
	FindByChannel(ctx context.Context, platform, externalChatID string) (*chat.Chat, error)
	// IsChatAdmin reports whether the person may change the games of the chat.
	// The app shows the admin controls from this answer, and the VK adapter
	// checks it again for every change — the flag in the browser is not a right.
	IsChatAdmin(ctx context.Context, chatID, userID int64) (bool, error)
}

// UserStore resolves the VK id of the person who opened the app to an internal
// User, creating it on first contact.
type UserStore interface {
	FindOrCreateUserByExternalID(ctx context.Context, platform, externalUserID, displayName string) (chat.User, error)
}

// IdentityStore maps internal users back to their VK ids: the lineup needs them
// to load avatars.
type IdentityStore interface {
	IdentitiesByUsers(ctx context.Context, platform string, userIDs []int64) (map[int64]string, error)
}

// EventStore is the games side of the app.
type EventStore interface {
	Get(ctx context.Context, chatID, eventID int64) (event.Event, error)
	ListUpcomingWithCounts(ctx context.Context, chatID int64, from time.Time, limit int) ([]event.EventSummary, error)
}

// BookingStore is the lineup side of the app: who is in the lineup and who is
// in the reserve.
type BookingStore interface {
	ListByEvent(ctx context.Context, eventID int64, statuses ...booking.Status) ([]booking.Booking, error)
}

// ScheduleStore holds the regular game times of a chat: the create form offers
// them as quick picks, the same way «/старт» takes the nearest one.
type ScheduleStore interface {
	List(ctx context.Context, chatID int64) ([]schedule.Slot, error)
}

// NameSource resolves the display name of a VK user.
type NameSource interface {
	GetUserName(ctx context.Context, userID int64) (string, error)
}

// PhotoSource resolves avatars of VK users.
type PhotoSource interface {
	UserPhotos(ctx context.Context, userIDs []int64) (map[int64]string, error)
}

// ChatSyncer shows a booking made in the app to the conversation and performs
// the changes an administrator makes: the line in the chat, the rewritten
// announcement and the rights check all live behind it, so the app and the bot
// take the same path. *vk.Service implements it.
type ChatSyncer interface {
	BookInChat(ctx context.Context, peerID, chatID, eventID int64, user chat.User) (booking.Booking, error)
	CancelInChat(ctx context.Context, peerID, chatID, eventID, userID int64) (booking.CancelResult, error)
	// StartGameInChat creates a game and opens its sign-up in the chat.
	StartGameInChat(ctx context.Context, peerID, userID int64, in event.CreateInput) (event.Event, error)
	// UpdateGameInChat applies an administrator's changes and rewrites the announcement.
	UpdateGameInChat(ctx context.Context, peerID, userID int64, in event.UpdateInput) (event.Event, error)
	// CancelGameInChat calls a game off and drops its bookings.
	CancelGameInChat(ctx context.Context, peerID, chatID, userID, eventID int64) (event.Event, error)
	// RemoveBookingInChat takes a participant out of a game.
	RemoveBookingInChat(ctx context.Context, peerID, chatID, userID, eventID, bookingID int64) (booking.CancelResult, error)
}

// Config carries the settings of the mini app.
type Config struct {
	// Base is the mount prefix without a trailing slash, e.g. "/app". The pages
	// build their URLs from it, so the mount point lives in one place.
	Base string
	// GroupID is the community id (VK_GROUP_ID). The diagnostic page pre-fills
	// it into the app_payload form.
	GroupID string
	// AppSecret is the protected key of the app (VK_APP_SECRET). When set, every
	// API call must carry a valid `sign` of its launch parameters — otherwise
	// anyone could send the vk_user_id of another person and book for them.
	// Without the key the check is skipped and the answer says so
	// (sign_checked=false): that is the development mode, and the app warns.
	AppSecret string
	// Location is the timezone the games are shown in.
	Location *time.Location
	// Now returns the current time; tests pin it.
	Now func() time.Time
	// Log receives page hits and rejected requests.
	Log *log.Logger
}

// Deps are the collaborators of the mini app: the same domain services the bot
// uses.
type Deps struct {
	Chats      ChatStore
	Users      UserStore
	Identities IdentityStore
	Events     EventStore
	Bookings   BookingStore
	Schedule   ScheduleStore
	Names      NameSource
	Photos     PhotoSource
	Chat       ChatSyncer
}

type handler struct {
	base      string
	groupID   string
	appSecret string
	loc       *time.Location
	now       func() time.Time
	log       *log.Logger
	pages     *template.Template
	files     http.Handler
	deps      Deps
}

// page is everything the HTML templates need from the server.
type page struct {
	// Base is the mount prefix the page builds its URLs from.
	Base string
	// GroupID pre-fills the app_payload form of the diagnostic page.
	GroupID string
	// ServedAt is the moment the server rendered the page (RFC3339, UTC). It is
	// the test marker of the diagnostic page: a fresh timestamp proves the
	// WebView got a live response from the Go binary rather than a stale page.
	ServedAt string
}

// New builds the mini app handler. Mount it on Config.Base + "/", so the pages
// and the API keep their URLs:
//
//	mux.Handle("GET /app/", app)
//	mux.Handle("POST /app/api/", app)
func New(cfg Config, deps Deps) http.Handler {
	if cfg.Base == "" {
		cfg.Base = "/app"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Location == nil {
		cfg.Location = defaultLocation
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	return &handler{
		base:      strings.TrimSuffix(cfg.Base, "/"),
		groupID:   cfg.GroupID,
		appSecret: strings.TrimSpace(cfg.AppSecret),
		loc:       cfg.Location,
		now:       cfg.Now,
		log:       cfg.Log,
		pages:     template.Must(template.ParseFS(assets, "app.html", "debug.html")),
		files:     http.FileServerFS(assets),
		deps:      deps,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == h.base, path == h.base+"/":
		h.servePage(w, r, "app.html")
	case path == h.base+"/debug", path == h.base+"/debug/":
		h.servePage(w, r, "debug.html")
	case path == h.base+"/api/state":
		h.apiState(w, r)
	case path == h.base+"/api/attend":
		h.bookingAction(w, r, true)
	case path == h.base+"/api/skip":
		h.bookingAction(w, r, false)
	case path == h.base+"/api/game":
		h.apiGameCreate(w, r)
	case path == h.base+"/api/game/edit":
		h.apiGameEdit(w, r)
	case path == h.base+"/api/game/cancel":
		h.apiGameCancel(w, r)
	case path == h.base+"/api/remove":
		h.apiRemoveBooking(w, r)
	case strings.HasPrefix(path, h.base+"/static/"):
		h.serveStatic(w, r)
	default:
		http.NotFound(w, r)
	}
}

// servePage renders one of the two pages. Every hit is logged with the raw query
// string: the log line is how the launch parameters VK actually sent are seen,
// and the diagnostic page exists for exactly that.
func (h *handler) servePage(w http.ResponseWriter, r *http.Request, name string) {
	h.log.Printf("miniapp: page hit path=%s query=%q ua=%q", r.URL.Path, r.URL.RawQuery, r.UserAgent())

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data := page{
		Base:     h.base,
		GroupID:  h.groupID,
		ServedAt: h.now().UTC().Format(time.RFC3339),
	}
	if err := h.pages.ExecuteTemplate(w, name, data); err != nil {
		h.log.Printf("miniapp: render %s: %v", name, err)
	}
}

// serveStatic serves the embedded files below <Base>/static/.
func (h *handler) serveStatic(w http.ResponseWriter, r *http.Request) {
	clone := r.Clone(r.Context())
	clone.URL.Path = strings.TrimPrefix(r.URL.Path, h.base)
	h.files.ServeHTTP(w, clone)
}

// launchParams are the values VK appends to the app address.
type launchParams struct {
	userID int64
	chatID int64
	// hash is the launch hash. Our own «Открыть приложение» button carries the
	// conversation in it, because VK does not pass vk_chat_id for a launch from
	// the bot keyboard.
	hash string
	// signed reports that the sign of the launch parameters is valid, checked
	// that the check was possible at all (the protected key is configured).
	signed  bool
	checked bool
}

// peerID is the conversation the app was opened from: vk_chat_id numbers a chat,
// and the «peer» in the launch hash names it directly — the app asks the bot's
// own chat_channels about either. Zero means the app does not know the chat.
func (lp launchParams) peerID() int64 {
	if lp.chatID != 0 {
		return conversationPeerIDMin + lp.chatID
	}
	return peerFromHash(lp.hash)
}

// peerFromHash reads what the app button put into the launch hash: «peer=<id>».
// Empty or foreign content means the button was an older one or the hash is not
// ours, and then the app can only tell the person to open it again.
func peerFromHash(hash string) int64 {
	for _, part := range strings.Split(strings.TrimPrefix(hash, "#"), "&") {
		name, value, ok := strings.Cut(part, "=")
		if !ok || name != "peer" {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || id < conversationPeerIDMin {
			return 0
		}
		return id
	}
	return 0
}

// launchParams reads the query and, when the protected key is configured,
// verifies the sign: without that check anyone could call the API with someone
// else's vk_user_id. A mismatch is answered with 403 and the request stops.
func (h *handler) launchParams(w http.ResponseWriter, r *http.Request) (launchParams, bool) {
	q := r.URL.Query()
	lp := launchParams{
		userID: atoi64(q.Get("vk_user_id")),
		chatID: atoi64(q.Get("vk_chat_id")),
		hash:   q.Get("hash"),
	}
	if h.appSecret != "" {
		lp.checked = true
		lp.signed = signMatches(q, h.appSecret)
		if !lp.signed {
			h.log.Printf("miniapp: rejected api call: bad sign (vk_user_id=%d vk_chat_id=%d)", lp.userID, lp.chatID)
			writeError(w, http.StatusForbidden, "подпись параметров запуска не совпала — откройте приложение заново из ВКонтакте")
			return launchParams{}, false
		}
	}
	return lp, true
}

// signMatches reproduces the VK algorithm: only the launch parameters with the
// vk_ prefix take part, sorted by name and joined as key=value pairs with the
// values in URL encoding, signed with the protected key of the app — HMAC-SHA256
// in URL-safe base64. Our own parameters (the conversation in the launch hash,
// for instance) are not signed and must not enter the string: the check would
// never pass again.
//
// Проверено на примере из документации VK: параметры и подпись оттуда дают
// ровно эту строку (см. signMatchesOfficialExample в тестах пакета).
func signMatches(q url.Values, secret string) bool {
	got := q.Get("sign")
	if got == "" || secret == "" {
		return false
	}

	keys := make([]string, 0, len(q))
	for k := range q {
		if strings.HasPrefix(k, "vk_") {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return false
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+encodeURIComponent(q.Get(k)))
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.Join(pairs, "&")))
	want := mac.Sum(nil)

	// VK sends the signature as URL-safe base64 without padding; the padded and
	// the standard alphabets are accepted too, so the comparison is on bytes.
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding,
	} {
		raw, err := enc.DecodeString(got)
		if err != nil {
			continue
		}
		if hmac.Equal(raw, want) {
			return true
		}
	}
	return false
}

// encodeURIComponent escapes a value the way the JavaScript reference
// implementation of the launch-parameter signature does: everything outside the
// unreserved set A-Za-z0-9-_.!~*'() is percent-encoded byte by byte.
func encodeURIComponent(s string) string {
	const unreserved = "-_.!~*'()"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case strings.IndexByte(unreserved, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// errNoChatContext, errNotConnected and errNoUser describe an app opened where
// it cannot work: outside a conversation, in a conversation the bot does not
// know, or without a user VK does not name. The first two need different hints,
// so they are different errors.
var (
	errNoChatContext = errors.New("launch parameters carry no chat")
	errNotConnected  = errors.New("conversation is not connected")
	errNoUser        = errors.New("launch parameters carry no user")
)

// resolve maps the launch parameters to the Chat and the internal User of the
// person who opened the app.
func (h *handler) resolve(ctx context.Context, lp launchParams) (chat.Chat, chat.User, error) {
	peer := lp.peerID()
	if peer == 0 {
		return chat.Chat{}, chat.User{}, errNoChatContext
	}
	ch, err := h.deps.Chats.FindByChannel(ctx, chat.PlatformVK, strconv.FormatInt(peer, 10))
	if err != nil {
		return chat.Chat{}, chat.User{}, err
	}
	if ch == nil {
		return chat.Chat{}, chat.User{}, errNotConnected
	}
	if lp.userID == 0 {
		return chat.Chat{}, chat.User{}, errNoUser
	}

	name := defaultPlayerName
	if h.deps.Names != nil {
		if resolved, err := h.deps.Names.GetUserName(ctx, lp.userID); err != nil {
			// Имя — не повод отказать: без него запись оформляется как «Участник».
			h.log.Printf("miniapp: resolve name of vk user %d: %v", lp.userID, err)
		} else if strings.TrimSpace(resolved) != "" {
			name = resolved
		}
	}
	u, err := h.deps.Users.FindOrCreateUserByExternalID(ctx, chat.PlatformVK, strconv.FormatInt(lp.userID, 10), name)
	if err != nil {
		return chat.Chat{}, chat.User{}, err
	}
	return *ch, u, nil
}

// apiState answers the whole screen in one request: the app keeps no state of
// its own and redraws from this answer after every action.
func (h *handler) apiState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "только GET")
		return
	}
	lp, ok := h.launchParams(w, r)
	if !ok {
		return
	}
	resp, err := h.state(r.Context(), lp)
	if err != nil {
		h.writeServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// state is what the app draws: the conversation, the person who opened it, and
// the games with their lineups.
type state struct {
	Chat   *chatView  `json:"chat,omitempty"`
	Me     *userView  `json:"me,omitempty"`
	Games  []gameView `json:"games"`
	Notice string     `json:"notice,omitempty"`
	// Admin says whether the person may change the games: the app draws the
	// create form and the per-game controls from it. Every change is checked
	// again on its way to the chat — the flag is only the interface.
	Admin bool `json:"admin"`
	// NextGame and Schedule pre-fill the create form; they come to administrators
	// only, because nobody else creates games.
	NextGame *nextGameView `json:"next_game,omitempty"`
	Schedule []slotView    `json:"schedule,omitempty"`
	// SignChecked says whether the launch parameters were verified at all, and
	// SignValid that the verification passed. The app warns when the check is off.
	SignChecked bool `json:"sign_checked"`
	SignValid   bool `json:"sign_valid"`
}

// nextGameView is what the create form is pre-filled with: the nearest regular
// slot of the chat (the one «/старт» would take) and the bot's defaults.
type nextGameView struct {
	Date     string `json:"date"` // 2006-01-02
	Time     string `json:"time"` // 15:04
	Title    string `json:"title"`
	Capacity int    `json:"capacity"`
}

// slotView is one regular game time of the chat: the form offers them as quick
// picks instead of making the administrator type the same time week after week.
type slotView struct {
	Weekday string `json:"weekday"` // «вс»
	Time    string `json:"time"`    // «10:00»
}

type chatView struct {
	Title string `json:"title"`
}

type userView struct {
	Name  string `json:"name"`
	Photo string `json:"photo,omitempty"`
	Me    bool   `json:"me,omitempty"`
	// BookingID lets an administrator take the person out of the game.
	BookingID int64 `json:"booking_id,omitempty"`
}

type gameView struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Location string `json:"location,omitempty"`
	StartsAt string `json:"starts_at"`
	// Date and Time are the same moment in the chat's timezone: the edit form
	// fills its date and time inputs from them, so the phone's zone never
	// shifts the game.
	Date      string     `json:"date"`
	Time      string     `json:"time"`
	When      string     `json:"when"`
	WhenFull  string     `json:"when_full"`
	Capacity  int        `json:"capacity"`
	Confirmed int        `json:"confirmed"`
	Waitlist  int        `json:"waitlist"`
	Free      int        `json:"free"`
	Lineup    []seatView `json:"lineup"`
	Reserve   []userView `json:"reserve"`
	Mine      *mineView  `json:"mine,omitempty"`
}

// seatView is one person in the lineup: the seat number never shifts, so the app
// shows the same numbers the chat does.
type seatView struct {
	Seat  int    `json:"seat"`
	Name  string `json:"name"`
	Photo string `json:"photo,omitempty"`
	Me    bool   `json:"me,omitempty"`
	// BookingID lets an administrator take the person out of the game.
	BookingID int64 `json:"booking_id,omitempty"`
}

// mineView is the state of the person who opened the app in one game.
type mineView struct {
	Status string `json:"status"` // confirmed | waitlist
	Seat   int    `json:"seat,omitempty"`
	Place  int    `json:"place,omitempty"` // position in the reserve
}

// state builds the whole answer. A conversation the app cannot use (opened not
// from a chat, or a chat the bot does not know) is not an error: the screen then
// shows a hint instead of games.
func (h *handler) state(ctx context.Context, lp launchParams) (state, error) {
	// An empty list, not null: the app draws from this answer directly.
	resp := state{Games: []gameView{}, SignChecked: lp.checked, SignValid: lp.signed}

	ch, me, err := h.resolve(ctx, lp)
	switch {
	case errors.Is(err, errNoChatContext):
		resp.Notice = "Приложение не знает, из какой беседы его открыли. Отправьте боту в беседе любую команду (например «/помощь»), чтобы клавиатура обновилась, и нажмите «Открыть приложение» ещё раз."
		return resp, nil
	case errors.Is(err, errNotConnected):
		resp.Notice = "Эта беседа ещё не подключена к боту: администратору нужно написать ему «подключить»."
		return resp, nil
	case errors.Is(err, errNoUser):
		resp.Notice = "ВКонтакте не назвал пользователя, поэтому записаться нельзя. Откройте приложение из беседы заново."
		return resp, nil
	case err != nil:
		return resp, err
	}
	resp.Chat = &chatView{Title: ch.Title}
	resp.Me = &userView{Name: me.DisplayName}

	// Права администратора: приложение рисует по ним формы, но каждое изменение
	// всё равно проверяется ещё раз на пути в беседу (см. adminLaunch).
	admin, err := h.deps.Chats.IsChatAdmin(ctx, ch.ID, me.ID)
	if err != nil {
		return resp, fmt.Errorf("check chat admin: %w", err)
	}
	resp.Admin = admin
	if admin {
		h.fillAdminForm(ctx, ch.ID, &resp)
	}

	games, err := h.deps.Events.ListUpcomingWithCounts(ctx, ch.ID, h.now().UTC(), gamesLimit)
	if err != nil {
		return resp, fmt.Errorf("list games: %w", err)
	}

	lineups := make([][]booking.Booking, 0, len(games))
	userIDs := make([]int64, 0, len(games)*4)
	for _, g := range games {
		bs, err := h.deps.Bookings.ListByEvent(ctx, g.ID, booking.StatusConfirmed, booking.StatusWaitlist)
		if err != nil {
			return resp, fmt.Errorf("list bookings of game %d: %w", g.ID, err)
		}
		for _, b := range bs {
			if b.UserID != nil {
				userIDs = append(userIDs, *b.UserID)
			}
		}
		lineups = append(lineups, bs)
	}

	photos, err := h.photos(ctx, userIDs)
	if err != nil {
		// Аватарки — украшение: без них список остаётся рабочим, и об этом
		// достаточно строки в логе.
		h.log.Printf("miniapp: avatars: %v", err)
	}

	resp.Games = make([]gameView, 0, len(games))
	for i, g := range games {
		resp.Games = append(resp.Games, h.gameView(g, lineups[i], me.ID, photos))
	}
	return resp, nil
}

// fillAdminForm adds what the create form needs: the nearest regular slot of the
// chat (the one «/старт» would take) and the bot's defaults for the title and the
// capacity. A chat without a schedule gets tomorrow at 19:00 — the same fallback
// the bot has.
func (h *handler) fillAdminForm(ctx context.Context, chatID int64, resp *state) {
	next := nextGameView{Title: defaultTitle, Capacity: defaultCapacity}

	day := h.now().In(h.loc).AddDate(0, 0, 1)
	hour, minute := defaultHour, 0

	if h.deps.Schedule != nil {
		slots, err := h.deps.Schedule.List(ctx, chatID)
		if err != nil {
			h.log.Printf("miniapp: list schedule: %v", err)
		}
		for _, slot := range slots {
			resp.Schedule = append(resp.Schedule, slotView{
				Weekday: weekdaysShort[slot.Weekday%7],
				Time:    slot.At(),
			})
		}
		if chosen, ok := schedule.Next(h.now(), slots, h.loc); ok {
			local := chosen.In(h.loc)
			day, hour, minute = local, local.Hour(), local.Minute()
		}
	}

	next.Date = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, h.loc).Format("2006-01-02")
	next.Time = fmt.Sprintf("%02d:%02d", hour, minute)
	resp.NextGame = &next
}

// photos resolves avatars of internal users: first their VK ids
// (user_identities), then the pictures (users.get). The ids are de-duplicated so
// one call covers the whole screen.
func (h *handler) photos(ctx context.Context, userIDs []int64) (map[int64]string, error) {
	// Без ВКонтакте (бот не настроен) аватарок нет: список рисует инициалы.
	if h.deps.Photos == nil {
		return nil, nil
	}
	seen := make(map[int64]bool, len(userIDs))
	ids := make([]int64, 0, len(userIDs))
	for _, id := range userIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	identities, err := h.deps.Identities.IdentitiesByUsers(ctx, chat.PlatformVK, ids)
	if err != nil {
		return nil, err
	}
	vkIDs := make([]int64, 0, len(identities))
	for _, external := range identities {
		if id, err := strconv.ParseInt(external, 10, 64); err == nil && id != 0 {
			vkIDs = append(vkIDs, id)
		}
	}
	if len(vkIDs) == 0 {
		return nil, nil
	}

	byVK, err := h.deps.Photos.UserPhotos(ctx, vkIDs)
	if err != nil {
		return nil, err
	}
	byUser := make(map[int64]string, len(byVK))
	for userID, external := range identities {
		id, err := strconv.ParseInt(external, 10, 64)
		if err != nil {
			continue
		}
		if photo := byVK[id]; photo != "" {
			byUser[userID] = photo
		}
	}
	return byUser, nil
}

// gameView renders one game: the lineup with seat numbers and avatars, the
// reserve in its order, and what the person who opened the app has in it.
func (h *handler) gameView(g event.EventSummary, booked []booking.Booking, meID int64, photos map[int64]string) gameView {
	local := g.StartsAt.In(h.loc)
	v := gameView{
		ID:        g.ID,
		Title:     g.Title,
		Location:  g.Location,
		StartsAt:  g.StartsAt.UTC().Format(time.RFC3339),
		Date:      local.Format("2006-01-02"),
		Time:      local.Format("15:04"),
		When:      shortWhen(local),
		WhenFull:  fullWhen(local),
		Capacity:  g.Capacity,
		Confirmed: g.Confirmed,
		Waitlist:  g.Waitlist,
		Free:      g.Free,
		Lineup:    make([]seatView, 0, len(booked)),
		Reserve:   make([]userView, 0, len(booked)),
	}

	place := 0
	for _, b := range booked {
		mine := b.UserID != nil && *b.UserID == meID
		switch b.Status {
		case booking.StatusConfirmed:
			seat := seatOf(b)
			v.Lineup = append(v.Lineup, seatView{
				Seat:      seat,
				Name:      b.PlayerName,
				Photo:     photoOf(b.UserID, photos),
				Me:        mine,
				BookingID: b.ID,
			})
			if mine {
				v.Mine = &mineView{Status: string(booking.StatusConfirmed), Seat: seat}
			}
		case booking.StatusWaitlist:
			place++
			v.Reserve = append(v.Reserve, userView{
				Name:      b.PlayerName,
				Photo:     photoOf(b.UserID, photos),
				Me:        mine,
				BookingID: b.ID,
			})
			if mine {
				v.Mine = &mineView{Status: string(booking.StatusWaitlist), Place: place}
			}
		}
	}
	return v
}

// bookingAction serves both «Записаться» and «Отписаться»: the two differ only in
// the call, and both answer with what happened. The app then re-reads the state,
// so a stale screen cannot survive a slow request.
func (h *handler) bookingAction(w http.ResponseWriter, r *http.Request, attend bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "нужен POST")
		return
	}
	lp, ok := h.launchParams(w, r)
	if !ok {
		return
	}
	eventID, err := eventIDFrom(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "не понятно, о какой игре речь")
		return
	}

	// Без бота записывать некуда: строка в чат и анонс — его работа.
	if h.deps.Chat == nil {
		writeError(w, http.StatusConflict, "бот не настроен на сервере: запись из приложения недоступна")
		return
	}

	ctx := r.Context()
	ch, me, err := h.resolve(ctx, lp)
	if err != nil {
		h.writeResolveError(w, err)
		return
	}

	ev, err := h.deps.Events.Get(ctx, ch.ID, eventID)
	switch {
	case errors.Is(err, event.ErrNotFound):
		writeError(w, http.StatusNotFound, "игра не найдена")
		return
	case err != nil:
		h.writeServerError(w, err)
		return
	}
	switch {
	case ev.Status == event.StatusCancelled:
		writeError(w, http.StatusConflict, "игра отменена")
		return
	case !ev.StartsAt.After(h.now()):
		writeError(w, http.StatusConflict, "игра уже прошла")
		return
	}

	if attend {
		h.attend(w, ctx, lp, ch.ID, ev.ID, me)
		return
	}
	h.skip(w, ctx, lp, ch.ID, ev.ID, me.ID)
}

// attend signs the person up: the same booking the «Иду» button makes, including
// the line in the chat and the rewritten announcement.
func (h *handler) attend(w http.ResponseWriter, ctx context.Context, lp launchParams, chatID, eventID int64, me chat.User) {
	b, err := h.deps.Chat.BookInChat(ctx, lp.peerID(), chatID, eventID, me)
	switch {
	case errors.Is(err, booking.ErrAlreadyBooked):
		writeError(w, http.StatusConflict, "вы уже записаны на эту игру")
		return
	case err != nil:
		h.writeServerError(w, err)
		return
	}
	h.log.Printf("miniapp: attend chat=%d game=%d user=%d status=%s", chatID, eventID, me.ID, b.Status)

	out := map[string]any{"ok": true, "status": string(b.Status), "seat_no": seatOf(b)}
	if b.Status == booking.StatusWaitlist {
		out["place"] = h.reservePlace(ctx, eventID, b.ID)
	}
	writeJSON(w, http.StatusOK, out)
}

// skip cancels the person's booking: the same cancellation the «Не иду» button
// makes, with the promotion it may cause.
func (h *handler) skip(w http.ResponseWriter, ctx context.Context, lp launchParams, chatID, eventID, userID int64) {
	res, err := h.deps.Chat.CancelInChat(ctx, lp.peerID(), chatID, eventID, userID)
	switch {
	case errors.Is(err, booking.ErrNotFound), errors.Is(err, booking.ErrNotActive):
		writeError(w, http.StatusConflict, "вы не записаны на эту игру")
		return
	case err != nil:
		h.writeServerError(w, err)
		return
	}
	h.log.Printf("miniapp: skip chat=%d game=%d user=%d", chatID, eventID, userID)

	out := map[string]any{"ok": true}
	if res.Promoted != nil {
		out["promoted"] = fmt.Sprintf("%d - %s", seatOf(*res.Promoted), res.Promoted.PlayerName)
	}
	writeJSON(w, http.StatusOK, out)
}

// reservePlace is the position of a fresh reserve booking in the queue.
func (h *handler) reservePlace(ctx context.Context, eventID, bookingID int64) int {
	queued, err := h.deps.Bookings.ListByEvent(ctx, eventID, booking.StatusWaitlist)
	if err != nil {
		h.log.Printf("miniapp: reserve position: %v", err)
		return 0
	}
	for i, b := range queued {
		if b.ID == bookingID {
			return i + 1
		}
	}
	return 0
}

// --- админские действия ---

// adminLaunch authenticates a request and checks that the person administers the
// chat: every action that changes a game goes through it, and the VK adapter
// checks the rights again on its side.
func (h *handler) adminLaunch(w http.ResponseWriter, r *http.Request) (launchParams, chat.Chat, chat.User, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "нужен POST")
		return launchParams{}, chat.Chat{}, chat.User{}, false
	}
	lp, ok := h.launchParams(w, r)
	if !ok {
		return launchParams{}, chat.Chat{}, chat.User{}, false
	}
	// Без бота менять игры негде: строку в чат и анонс делает его адаптер.
	if h.deps.Chat == nil {
		writeError(w, http.StatusConflict, "бот не настроен на сервере: менять игры из приложения нельзя")
		return launchParams{}, chat.Chat{}, chat.User{}, false
	}

	ctx := r.Context()
	ch, me, err := h.resolve(ctx, lp)
	if err != nil {
		h.writeResolveError(w, err)
		return launchParams{}, chat.Chat{}, chat.User{}, false
	}

	admin, err := h.deps.Chats.IsChatAdmin(ctx, ch.ID, me.ID)
	if err != nil {
		h.writeServerError(w, err)
		return launchParams{}, chat.Chat{}, chat.User{}, false
	}
	if !admin {
		h.log.Printf("miniapp: admin action refused: chat=%d user=%d", ch.ID, me.ID)
		writeError(w, http.StatusForbidden, "это может делать только администратор беседы")
		return launchParams{}, chat.Chat{}, chat.User{}, false
	}
	if h.appSecret == "" {
		// Подпись не проверяется: в разработке допустимо, но в логе остаётся.
		h.log.Printf("miniapp: admin action without sign check: chat=%d user=%d", ch.ID, me.ID)
	}
	return lp, ch, me, true
}

// gameForm is what the create and edit forms post: the date and the time as the
// input fields give them, the rest as text.
type gameForm struct {
	EventID  int64  `json:"event_id"`
	Title    string `json:"title"`
	Date     string `json:"date"` // 2006-01-02
	Time     string `json:"time"` // 15:04
	Location string `json:"location"`
	Capacity int    `json:"capacity"`
}

// startsAt reads the date and the time in the chat's timezone: a phone may be in
// any zone, while the game time belongs to the chat.
func (h *handler) startsAt(f gameForm) (time.Time, error) {
	date := strings.TrimSpace(f.Date)
	clock := strings.TrimSpace(f.Time)
	if clock == "" {
		clock = fmt.Sprintf("%02d:00", defaultHour)
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, h.loc)
	if err != nil {
		return time.Time{}, errors.New("не понял дату и время: нужны 2026-09-27 и 19:00")
	}
	return at, nil
}

func (h *handler) titleOr(title string) string {
	if strings.TrimSpace(title) == "" {
		return defaultTitle
	}
	return strings.TrimSpace(title)
}

func capacityOr(capacity int) int {
	if capacity < 1 {
		return defaultCapacity
	}
	return capacity
}

// decodeForm reads the JSON body of an admin form.
func decodeForm(w http.ResponseWriter, r *http.Request, form any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(form); err != nil {
		writeError(w, http.StatusBadRequest, "не понял запрос")
		return false
	}
	return true
}

// writeAdminError turns the domain and adapter errors of an admin action into
// answers the app can show as is.
func (h *handler) writeAdminError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chat.ErrNotAdmin):
		writeError(w, http.StatusForbidden, "это может делать только администратор беседы")
	case errors.Is(err, event.ErrCapacityBelowConfirmed):
		writeError(w, http.StatusConflict, "меньше мест, чем уже занято, поставить нельзя — снимите кого-нибудь")
	case errors.Is(err, event.ErrAlreadyCancelled):
		writeError(w, http.StatusConflict, "игра уже отменена")
	case errors.Is(err, event.ErrNotFound):
		writeError(w, http.StatusNotFound, "игра не найдена")
	case errors.Is(err, booking.ErrNotFound), errors.Is(err, booking.ErrNotActive):
		writeError(w, http.StatusConflict, "такой записи уже нет")
	default:
		h.writeServerError(w, err)
	}
}

// apiGameCreate starts a game: the announcement goes to the chat and the sign-up
// keyboard appears under the input — the same as «/старт».
func (h *handler) apiGameCreate(w http.ResponseWriter, r *http.Request) {
	lp, ch, me, ok := h.adminLaunch(w, r)
	if !ok {
		return
	}
	var form gameForm
	if !decodeForm(w, r, &form) {
		return
	}
	at, err := h.startsAt(form)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ev, err := h.deps.Chat.StartGameInChat(r.Context(), lp.peerID(), me.ID, event.CreateInput{
		ChatID:   ch.ID,
		StartsAt: at,
		Title:    h.titleOr(form.Title),
		Location: strings.TrimSpace(form.Location),
		Capacity: capacityOr(form.Capacity),
	})
	if err != nil {
		h.writeAdminError(w, err)
		return
	}
	h.log.Printf("miniapp: game %d started by user %d (%s)", ev.ID, me.ID, at.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "event_id": ev.ID, "when": fullWhen(ev.StartsAt.In(h.loc)),
	})
}

// apiGameEdit moves or re-arranges a game.
func (h *handler) apiGameEdit(w http.ResponseWriter, r *http.Request) {
	lp, ch, me, ok := h.adminLaunch(w, r)
	if !ok {
		return
	}
	var form gameForm
	if !decodeForm(w, r, &form) {
		return
	}
	if form.EventID <= 0 {
		writeError(w, http.StatusBadRequest, "не понятно, какую игру менять")
		return
	}
	at, err := h.startsAt(form)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	updated, err := h.deps.Chat.UpdateGameInChat(r.Context(), lp.peerID(), me.ID, event.UpdateInput{
		ChatID:   ch.ID,
		EventID:  form.EventID,
		StartsAt: at,
		Title:    h.titleOr(form.Title),
		Location: strings.TrimSpace(form.Location),
		Capacity: capacityOr(form.Capacity),
	})
	if err != nil {
		h.writeAdminError(w, err)
		return
	}
	h.log.Printf("miniapp: game %d edited by user %d (%s)", updated.ID, me.ID, at.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "event_id": updated.ID, "when": fullWhen(updated.StartsAt.In(h.loc)),
	})
}

// apiGameCancel calls a game off and drops its bookings.
func (h *handler) apiGameCancel(w http.ResponseWriter, r *http.Request) {
	lp, ch, me, ok := h.adminLaunch(w, r)
	if !ok {
		return
	}
	eventID, err := eventIDFrom(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "не понятно, какую игру отменять")
		return
	}

	cancelled, err := h.deps.Chat.CancelGameInChat(r.Context(), lp.peerID(), ch.ID, me.ID, eventID)
	if err != nil {
		h.writeAdminError(w, err)
		return
	}
	h.log.Printf("miniapp: game %d cancelled by user %d", cancelled.ID, me.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "event_id": cancelled.ID})
}

// apiRemoveBooking takes a participant out of a game: the chat sees whose seat it
// was and who came up from the reserve.
func (h *handler) apiRemoveBooking(w http.ResponseWriter, r *http.Request) {
	lp, ch, me, ok := h.adminLaunch(w, r)
	if !ok {
		return
	}

	var body struct {
		EventID   int64 `json:"event_id"`
		BookingID int64 `json:"booking_id"`
	}
	if !decodeForm(w, r, &body) {
		return
	}
	if body.EventID <= 0 || body.BookingID <= 0 {
		writeError(w, http.StatusBadRequest, "не понятно, кого снимать")
		return
	}

	res, err := h.deps.Chat.RemoveBookingInChat(r.Context(), lp.peerID(), ch.ID, me.ID, body.EventID, body.BookingID)
	if err != nil {
		h.writeAdminError(w, err)
		return
	}
	h.log.Printf("miniapp: booking %d removed from game %d by user %d", body.BookingID, body.EventID, me.ID)

	out := map[string]any{"ok": true}
	if res.Promoted != nil {
		out["promoted"] = fmt.Sprintf("%d - %s", seatOf(*res.Promoted), res.Promoted.PlayerName)
	}
	writeJSON(w, http.StatusOK, out)
}

// eventIDFrom reads the game an action belongs to: the app posts it as JSON, but
// a plain ?event_id= works too — так проще проверять руками.
func eventIDFrom(r *http.Request) (int64, error) {
	if v := r.URL.Query().Get("event_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("event_id: %q", v)
		}
		return id, nil
	}

	var body struct {
		EventID int64 `json:"event_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		return 0, fmt.Errorf("read body: %w", err)
	}
	if body.EventID <= 0 {
		return 0, errors.New("event_id is missing")
	}
	return body.EventID, nil
}

func atoi64(s string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return id
}

// seatOf is the seat number of a booking; a confirmed booking always has one,
// the zero stands for "no seat".
func seatOf(b booking.Booking) int {
	if b.SeatNo == nil {
		return 0
	}
	return *b.SeatNo
}

// photoOf is the avatar of the person behind a booking. A guest (a booking
// without a user) and a person without a picture have none, and the app draws
// initials instead.
func photoOf(userID *int64, photos map[int64]string) string {
	if userID == nil {
		return ""
	}
	return photos[*userID]
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Ответ уже начал уходить: сказать клиенту нечего, остаётся лог.
		log.Printf("miniapp: encode response: %v", err)
	}
}

// writeError answers the app with a message it can show as is.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (h *handler) writeResolveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoChatContext):
		writeError(w, http.StatusConflict, "приложение открыто не из беседы — записи здесь нет")
	case errors.Is(err, errNotConnected):
		writeError(w, http.StatusConflict,
			"беседа не подключена к боту: администратору нужно написать ему «подключить»")
	case errors.Is(err, errNoUser):
		writeError(w, http.StatusBadRequest, "ВКонтакте не назвал пользователя — откройте приложение заново")
	default:
		h.writeServerError(w, err)
	}
}

func (h *handler) writeServerError(w http.ResponseWriter, err error) {
	h.log.Printf("miniapp: %v", err)
	writeError(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
}

// Russian weekday and month names: the app speaks the language of the chat.
var (
	weekdaysFull  = [...]string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
	weekdaysShort = [...]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}
	monthsGen     = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня",
		"июля", "августа", "сентября", "октября", "ноября", "декабря"}
)

// shortWhen renders a game time the way the bot lists games: «вс, 27.09, 10:00».
func shortWhen(t time.Time) string {
	return fmt.Sprintf("%s, %02d.%02d, %s", weekdaysShort[t.Weekday()], t.Day(), int(t.Month()), t.Format("15:04"))
}

// fullWhen renders it the way the announcement speaks: «воскресенье, 27 сентября, 10:00».
func fullWhen(t time.Time) string {
	return fmt.Sprintf("%s, %d %s, %s", weekdaysFull[t.Weekday()], t.Day(), monthsGen[int(t.Month())-1], t.Format("15:04"))
}
