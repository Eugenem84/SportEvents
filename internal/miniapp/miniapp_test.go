package miniapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"sportevents.local/internal/booking"
	"sportevents.local/internal/chat"
	"sportevents.local/internal/event"
	"sportevents.local/internal/miniapp"
	"sportevents.local/internal/schedule"
)

// These tests exercise both pages of the mini app without VK and without
// PostgreSQL: the interface draws the lineup from the domain stores, books
// through the messenger side, and the diagnostic page stays where it was — only
// on its own URL now.

// --- fakes ---

type fakeChats struct {
	chat *chat.Chat
	// admin is what IsChatAdmin answers: the admin tests flip it.
	admin bool
}

func (f *fakeChats) FindByChannel(_ context.Context, platform, externalChatID string) (*chat.Chat, error) {
	if f.chat == nil {
		return nil, nil
	}
	return f.chat, nil
}

func (f *fakeChats) IsChatAdmin(_ context.Context, chatID, userID int64) (bool, error) {
	return f.admin, nil
}

func (f *fakeChats) FindOrCreateUserByExternalID(_ context.Context, platform, externalUserID, displayName string) (chat.User, error) {
	return chat.User{ID: 7, DisplayName: displayName}, nil
}

// IdentitiesByUsers invents a VK id from the internal one: the app only needs
// the mapping to exist to ask for an avatar.
func (f *fakeChats) IdentitiesByUsers(_ context.Context, platform string, userIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(userIDs))
	for _, id := range userIDs {
		out[id] = strconv.FormatInt(1000+id, 10)
	}
	return out, nil
}

type fakeEvents struct {
	games []event.EventSummary
	byID  map[int64]event.Event
}

func (f *fakeEvents) Get(_ context.Context, chatID, eventID int64) (event.Event, error) {
	ev, ok := f.byID[eventID]
	if !ok {
		return event.Event{}, event.ErrNotFound
	}
	return ev, nil
}

func (f *fakeEvents) ListUpcomingWithCounts(_ context.Context, chatID int64, from time.Time, limit int) ([]event.EventSummary, error) {
	return f.games, nil
}

type fakeBookings struct {
	byEvent map[int64][]booking.Booking
}

func (f *fakeBookings) ListByEvent(_ context.Context, eventID int64, statuses ...booking.Status) ([]booking.Booking, error) {
	want := make(map[booking.Status]bool, len(statuses))
	for _, s := range statuses {
		want[s] = true
	}
	var out []booking.Booking
	for _, b := range f.byEvent[eventID] {
		if want[b.Status] {
			out = append(out, b)
		}
	}
	return out, nil
}

type fakeNames struct {
	name string
	err  error
}

func (f *fakeNames) GetUserName(_ context.Context, userID int64) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.name, nil
}

type fakePhotos struct {
	photos map[int64]string
	calls  int
}

func (f *fakePhotos) UserPhotos(_ context.Context, userIDs []int64) (map[int64]string, error) {
	f.calls++
	return f.photos, nil
}

type fakeSyncer struct {
	peerID    int64
	chatID    int64
	user      chat.User
	booked    []int64
	bookSeat  int
	bookGuest string
	bookErr   error
	bookRes   booking.Booking
	cancel    booking.CancelResult
	cancelErr error

	// Админские действия: что просили и что адаптер ответил.
	adminUser     int64
	started       []event.CreateInput
	startRes      event.Event
	startErr      error
	edited        []event.UpdateInput
	editRes       event.Event
	editErr       error
	cancelled     []int64
	cancelGameRes event.Event
	cancelGameErr error
	removed       [][2]int64
	removeRes     booking.CancelResult
	removeErr     error
}

func (f *fakeSyncer) BookInChat(_ context.Context, peerID, chatID, eventID int64, by chat.User, seat int, guest string) (booking.Booking, error) {
	f.peerID, f.chatID, f.user = peerID, chatID, by
	f.booked = append(f.booked, eventID)
	f.bookSeat, f.bookGuest = seat, guest
	return f.bookRes, f.bookErr
}

func (f *fakeSyncer) CancelInChat(_ context.Context, peerID, chatID, eventID, userID int64) (booking.CancelResult, error) {
	f.peerID, f.chatID = peerID, chatID
	return f.cancel, f.cancelErr
}

func (f *fakeSyncer) StartGameInChat(_ context.Context, peerID, userID int64, in event.CreateInput) (event.Event, error) {
	f.peerID, f.adminUser = peerID, userID
	f.started = append(f.started, in)
	if f.startErr != nil {
		return event.Event{}, f.startErr
	}
	if f.startRes.ID != 0 {
		return f.startRes, nil
	}
	return event.Event{
		ID: 100, ChatID: in.ChatID, StartsAt: in.StartsAt, Title: in.Title,
		Location: in.Location, Capacity: in.Capacity, Status: event.StatusScheduled,
	}, nil
}

func (f *fakeSyncer) UpdateGameInChat(_ context.Context, peerID, userID int64, in event.UpdateInput) (event.Event, error) {
	f.peerID, f.adminUser = peerID, userID
	f.edited = append(f.edited, in)
	if f.editErr != nil {
		return event.Event{}, f.editErr
	}
	if f.editRes.ID != 0 {
		return f.editRes, nil
	}
	return event.Event{
		ID: in.EventID, ChatID: in.ChatID, StartsAt: in.StartsAt, Title: in.Title,
		Location: in.Location, Capacity: in.Capacity, Status: event.StatusScheduled,
	}, nil
}

func (f *fakeSyncer) CancelGameInChat(_ context.Context, peerID, chatID, userID, eventID int64) (event.Event, error) {
	f.peerID, f.chatID, f.adminUser = peerID, chatID, userID
	f.cancelled = append(f.cancelled, eventID)
	if f.cancelGameErr != nil {
		return event.Event{}, f.cancelGameErr
	}
	if f.cancelGameRes.ID != 0 {
		return f.cancelGameRes, nil
	}
	return event.Event{ID: eventID, ChatID: chatID, Status: event.StatusCancelled}, nil
}

func (f *fakeSyncer) RemoveBookingInChat(_ context.Context, peerID, chatID, userID, eventID, bookingID int64) (booking.CancelResult, error) {
	f.peerID, f.chatID, f.adminUser = peerID, chatID, userID
	f.removed = append(f.removed, [2]int64{eventID, bookingID})
	if f.removeErr != nil {
		return booking.CancelResult{}, f.removeErr
	}
	return f.removeRes, nil
}

// fakeSchedule is the regular game times of a chat.
type fakeSchedule struct {
	slots []schedule.Slot
	err   error
}

func (f *fakeSchedule) List(_ context.Context, chatID int64) ([]schedule.Slot, error) {
	return f.slots, f.err
}

// --- harness ---

type harness struct {
	app      http.Handler
	chats    *fakeChats
	events   *fakeEvents
	bookings *fakeBookings
	schedule *fakeSchedule
	names    *fakeNames
	photos   *fakePhotos
	sync     *fakeSyncer
	logs     *bytes.Buffer
}

// harnessNow is the pinned clock: the tests assert on the rendered time.
func harnessNow() time.Time {
	return time.Date(2026, 9, 25, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
}

func newHarness(t *testing.T, chatLinked bool) *harness {
	return newHarnessWithSecret(t, chatLinked, "")
}

// newHarnessWithSecret builds the handler; appSecret turns the sign check on.
func newHarnessWithSecret(t *testing.T, chatLinked bool, appSecret string) *harness {
	t.Helper()
	h := &harness{
		chats:    &fakeChats{admin: true},
		events:   &fakeEvents{byID: map[int64]event.Event{}},
		bookings: &fakeBookings{byEvent: map[int64][]booking.Booking{}},
		schedule: &fakeSchedule{},
		names:    &fakeNames{name: "Евгений Мёдов"},
		photos:   &fakePhotos{photos: map[int64]string{}},
		sync:     &fakeSyncer{},
		logs:     &bytes.Buffer{},
	}
	if chatLinked {
		h.chats.chat = &chat.Chat{ID: 1, Title: "Волейбол Иваново"}
	}
	h.app = miniapp.New(miniapp.Config{
		Base:      "/app",
		GroupID:   "241346632",
		AppSecret: appSecret,
		Now:       harnessNow,
		Log:       log.New(h.logs, "", 0),
	}, miniapp.Deps{
		Chats:      h.chats,
		Users:      h.chats,
		Identities: h.chats,
		Events:     h.events,
		Bookings:   h.bookings,
		Schedule:   h.schedule,
		Names:      h.names,
		Photos:     h.photos,
		Chat:       h.sync,
	})
	return h
}

// newHarnessWithoutBot is the server without VK configured: no names, no avatars
// and nobody to show a booking to.
func newHarnessWithoutBot(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, true)
	h.sync = nil
	h.app = miniapp.New(miniapp.Config{
		Base:    "/app",
		GroupID: "241346632",
		Now:     harnessNow,
		Log:     log.New(h.logs, "", 0),
	}, miniapp.Deps{
		Chats:      h.chats,
		Users:      h.chats,
		Identities: h.chats,
		Events:     h.events,
		Bookings:   h.bookings,
	})
	return h
}

// seedGame fills the stores with one game: two in the lineup (one of them is the
// person who opened the app) and one in the reserve.
func seedGame(h *harness) {
	game := event.Event{
		ID: 7, ChatID: 1, Title: "Волейбол", Location: "СК «Спартак»",
		StartsAt: harnessNow().Add(24 * time.Hour), Capacity: 12, Status: event.StatusScheduled,
	}
	h.events.games = []event.EventSummary{{Event: game, Confirmed: 2, Waitlist: 1, Free: 10}}
	h.events.byID[7] = game

	seat := func(n int) *int { return &n }
	user := func(id int64) *int64 { return &id }
	h.bookings.byEvent[7] = []booking.Booking{
		{ID: 100, EventID: 7, PlayerName: "Евгений Мёдов", UserID: user(7), SeatNo: seat(1), Status: booking.StatusConfirmed},
		{ID: 101, EventID: 7, PlayerName: "Пётр", SeatNo: seat(2), Status: booking.StatusConfirmed},
		{ID: 102, EventID: 7, PlayerName: "Иван", UserID: user(9), Status: booking.StatusWaitlist},
	}
	h.photos.photos = map[int64]string{
		1007: "https://sun.example/evgeny.jpg",
		1009: "https://sun.example/ivan.jpg",
	}
}

func do(t *testing.T, h http.Handler, method, target, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, reader))
	return rec.Code, rec.Body.String()
}

// --- страницы ---

func TestAppPageIsServed(t *testing.T) {
	h := newHarness(t, true)

	code, body := do(t, h.app, http.MethodGet, "/app/", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if strings.Contains(body, "Тестовый вывод") {
		t.Error("the interface must not be the diagnostic page")
	}
	for _, want := range []string{
		"/app/static/vk-bridge.min.js", // бридж из бинаря, без CDN
		"api('state')",                 // страница знает, куда спрашивать состояние
		"/app/debug/",                  // и где теперь живёт диагностика
		"Записаться",                   // кнопку записи рисует скрипт
		"Создать игру",                 // админская форма создания
		"Отменить игру",                // и отмена игры
		"game/edit",                    // правка игры
		"remove",                       // снятие участника
		"Добавить человека",            // форма для гостя, которого нет в чате
		"Записать гостя",
		"guest", // её ручка
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

// Диагностика переехала на свою ссылку: все проверки запуска остались там.
func TestDebugPageKeepsTheDiagnostics(t *testing.T) {
	h := newHarness(t, true)

	code, body := do(t, h.app, http.MethodGet,
		"/app/debug/?vk_user_id=42&vk_chat_id=47&vk_platform=mobile_android", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	for _, want := range []string{
		"Тестовый вывод",       // время сервера и метка JS
		"2026-09-25T09:00:00Z", // штамп рендера: часы тестов зафиксированы
		`value="241346632"`,    // VK_GROUP_ID в форме app_payload
		"location.search",      // сырой запрос запуска
		"VKWebAppGetLaunchParams",
		"VKWebAppSendPayload",
		"/app/static/vk-bridge.min.js",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("debug page does not contain %q", want)
		}
	}
}

func TestBridgeBundleIsServedFromBinary(t *testing.T) {
	h := newHarness(t, true)

	code, body := do(t, h.app, http.MethodGet, "/app/static/vk-bridge.min.js", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if !strings.Contains(body, "window.vkBridge") {
		t.Error("bundle does not define window.vkBridge")
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	h := newHarness(t, true)

	if code, _ := do(t, h.app, http.MethodGet, "/app/nope", ""); code != http.StatusNotFound {
		t.Fatalf("status: %d, want 404", code)
	}
}

// Заход на страницу пишется в лог вместе с сырым запросом: именно по этой
// строке видно, что VK положил в адрес запуска.
func TestPageHitsAreLoggedWithLaunchParams(t *testing.T) {
	h := newHarness(t, true)

	do(t, h.app, http.MethodGet, "/app/debug/?vk_user_id=42&vk_chat_id=47", "")

	logs := h.logs.String()
	if !strings.Contains(logs, "path=/app/debug/") {
		t.Fatalf("log: %q", logs)
	}
	if !strings.Contains(logs, `query="vk_user_id=42&vk_chat_id=47"`) {
		t.Fatalf("launch parameters must be logged as is: %q", logs)
	}
}

// --- состояние ---

// stateJSON mirrors the wire format the app draws from: the test decodes into it
// rather than into the server types, because what the browser sees is the
// contract.
type stateJSON struct {
	Notice      string `json:"notice"`
	SignChecked bool   `json:"sign_checked"`
	SignValid   bool   `json:"sign_valid"`
	Chat        *struct {
		Title string `json:"title"`
	} `json:"chat"`
	Me *struct {
		Name string `json:"name"`
	} `json:"me"`
	Games []struct {
		ID       int64  `json:"id"`
		When     string `json:"when"`
		WhenFull string `json:"when_full"`
		Free     int    `json:"free"`
		Lineup   []struct {
			Seat  int    `json:"seat"`
			Name  string `json:"name"`
			Photo string `json:"photo"`
			Me    bool   `json:"me"`
		} `json:"lineup"`
		Reserve []struct {
			Name string `json:"name"`
		} `json:"reserve"`
		Mine *struct {
			Status string `json:"status"`
			Seat   int    `json:"seat"`
			Place  int    `json:"place"`
		} `json:"mine"`
	} `json:"games"`
}

func decodeState(t *testing.T, body string) stateJSON {
	t.Helper()
	var s stateJSON
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("state json: %v (%q)", err, body)
	}
	return s
}

// Состав приходит с номерами мест, именами и аватарками: гости без профиля
// остаются с инициалами, а «я» отмечен отдельно.
func TestStateShowsLineupWithSeatsAndAvatars(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)

	code, body := do(t, h.app, http.MethodGet,
		"/app/api/state?vk_user_id=42&vk_chat_id=47&vk_platform=mobile_android", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}

	got := decodeState(t, body)
	if got.Chat == nil || got.Chat.Title != "Волейбол Иваново" {
		t.Fatalf("chat: %+v", got.Chat)
	}
	if got.Me == nil || got.Me.Name != "Евгений Мёдов" {
		t.Fatalf("me: %+v", got.Me)
	}
	if got.Notice != "" {
		t.Fatalf("unexpected notice: %q", got.Notice)
	}
	if len(got.Games) != 1 {
		t.Fatalf("games: %+v", got.Games)
	}

	g := got.Games[0]
	if !strings.Contains(g.WhenFull, "26 сентября, 12:00") {
		t.Errorf("full when: %q", g.WhenFull)
	}
	if !strings.Contains(g.When, "26.09, 12:00") {
		t.Errorf("short when: %q", g.When)
	}
	if g.Free != 10 {
		t.Errorf("free: %d", g.Free)
	}

	if len(g.Lineup) != 2 {
		t.Fatalf("lineup: %+v", g.Lineup)
	}
	first := g.Lineup[0]
	if first.Seat != 1 || first.Name != "Евгений Мёдов" || !first.Me {
		t.Errorf("first seat: %+v", first)
	}
	if first.Photo != "https://sun.example/evgeny.jpg" {
		t.Errorf("avatar must come from VK: %q", first.Photo)
	}
	guest := g.Lineup[1]
	if guest.Seat != 2 || guest.Name != "Пётр" || guest.Me {
		t.Errorf("second seat: %+v", guest)
	}
	if guest.Photo != "" {
		t.Errorf("a guest has no avatar: %q", guest.Photo)
	}

	if len(g.Reserve) != 1 || g.Reserve[0].Name != "Иван" {
		t.Errorf("reserve: %+v", g.Reserve)
	}
	if g.Mine == nil || g.Mine.Status != "confirmed" || g.Mine.Seat != 1 {
		t.Errorf("mine: %+v", g.Mine)
	}

	// Аватарки спрашивают одним запросом на весь экран, а не по одной на игру.
	if h.photos.calls != 1 {
		t.Errorf("users.get calls: %d, want 1", h.photos.calls)
	}
}

// Приложение, открытое не из беседы, показывает подсказку, а не пустой список.
func TestStateWithoutChatContextExplains(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)

	code, body := do(t, h.app, http.MethodGet, "/app/api/state?vk_user_id=42", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}

	got := decodeState(t, body)
	if !strings.Contains(got.Notice, "не знает") {
		t.Fatalf("notice: %q", got.Notice)
	}
	if got.Chat != nil || len(got.Games) != 0 {
		t.Fatalf("no chat, no games: %+v", got)
	}
}

// Кнопка «Открыть приложение» несёт беседу в хеше: при запуске с клавиатуры VK
// не передаёт vk_chat_id, поэтому без него приложение спрашивает сервер про peer
// из хеша.
func TestStateResolvesChatFromLaunchHash(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)

	code, body := do(t, h.app, http.MethodGet,
		"/app/api/state?vk_user_id=42&vk_platform=mobile_android&hash=peer%3D2000000047", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}

	got := decodeState(t, body)
	if got.Chat == nil || got.Chat.Title != "Волейбол Иваново" {
		t.Fatalf("chat from the hash: %+v (notice %q)", got.Chat, got.Notice)
	}
}

// Чужой хеш беседу не подменяет: без номера беседы остаётся подсказка.
func TestStateIgnoresForeignHash(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)

	code, body := do(t, h.app, http.MethodGet,
		"/app/api/state?vk_user_id=42&hash=screen%3Dsettings", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}

	got := decodeState(t, body)
	if got.Chat != nil || !strings.Contains(got.Notice, "не знает") {
		t.Fatalf("chat: %+v, notice: %q", got.Chat, got.Notice)
	}
}

// Беседа, которую бот не знает: подсказка про «подключить».
func TestStateOfUnconnectedChatExplains(t *testing.T) {
	h := newHarness(t, false)

	code, body := do(t, h.app, http.MethodGet, "/app/api/state?vk_user_id=42&vk_chat_id=47", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}

	got := decodeState(t, body)
	if !strings.Contains(got.Notice, "не подключена") {
		t.Fatalf("notice: %q", got.Notice)
	}
}

// --- подпись параметров запуска ---

// Пока защищённый ключ не задан, приложение честно говорит, что подпись не
// проверяется: иначе выглядело бы, будто проверка есть.
func TestStateSaysWhenSignIsNotChecked(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)

	code, body := do(t, h.app, http.MethodGet, "/app/api/state?vk_user_id=42&vk_chat_id=47", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}

	got := decodeState(t, body)
	if got.SignChecked || got.SignValid {
		t.Fatalf("sign flags: checked=%v valid=%v", got.SignChecked, got.SignValid)
	}
}

// С ключом вызовы без верной подписи отклоняются: иначе кто угодно мог бы
// записать другого человека, подставив его vk_user_id.
func TestSignIsRequiredWhenTheKeyIsConfigured(t *testing.T) {
	// The signature below is computed over the same parameters and the same
	// secret by an independent implementation (python hmac/sha256/base64):
	//   base64(hmac_sha256("test-secret",
	//     "vk_chat_id=47&vk_platform=mobile_android&vk_user_id=42"))
	const signed = "vk_user_id=42&vk_chat_id=47&vk_platform=mobile_android" +
		"&sign=9sWULBT%2By%2FyuFufvG2%2FW8fJBCuP3lUzf1Q5clW4egT8%3D"

	h := newHarnessWithSecret(t, true, "test-secret")
	seedGame(h)

	if code, _ := do(t, h.app, http.MethodGet, "/app/api/state?vk_user_id=42&vk_chat_id=47", ""); code != http.StatusForbidden {
		t.Errorf("without a sign: status %d, want 403", code)
	}
	if code, _ := do(t, h.app, http.MethodGet, "/app/api/state?vk_user_id=42&vk_chat_id=47&sign=nope", ""); code != http.StatusForbidden {
		t.Errorf("with a broken sign: status %d, want 403", code)
	}

	code, body := do(t, h.app, http.MethodGet, "/app/api/state?"+signed, "")
	if code != http.StatusOK {
		t.Fatalf("with a valid sign: status %d (%s)", code, body)
	}
	got := decodeState(t, body)
	if !got.SignChecked || !got.SignValid {
		t.Fatalf("sign flags: checked=%v valid=%v", got.SignChecked, got.SignValid)
	}
}

// --- запись из приложения ---

func TestAttendBooksAndTellsTheChat(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.sync.bookRes = booking.Booking{
		ID: 200, EventID: 7, PlayerName: "Евгений Мёдов",
		Status: booking.StatusConfirmed, SeatNo: intPtr(3),
	}

	code, body := do(t, h.app, http.MethodPost,
		"/app/api/attend?vk_user_id=42&vk_chat_id=47", `{"event_id":7}`)
	if code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if !strings.Contains(body, `"seat_no":3`) {
		t.Errorf("answer: %s", body)
	}

	// Беседа получает ту же строку и тот же анонс, что после кнопки «Иду»:
	// приложение только передаёт, чем всё закончилось.
	if len(h.sync.booked) != 1 || h.sync.booked[0] != 7 {
		t.Fatalf("booked games: %+v", h.sync.booked)
	}
	if h.sync.peerID != 2000000047 {
		t.Errorf("peer id: %d, want 2000000047 (vk_chat_id 47 с префиксом беседы)", h.sync.peerID)
	}
	if h.sync.chatID != 1 {
		t.Errorf("internal chat: %d", h.sync.chatID)
	}
	if h.sync.user.ID != 7 {
		t.Errorf("user: %+v", h.sync.user)
	}
}

// Полная игра отправляет в резерв, и приложение отвечает местом в очереди.
func TestAttendGoesToTheReserveWhenFull(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.sync.bookRes = booking.Booking{
		ID: 201, EventID: 7, PlayerName: "Евгений Мёдов", Status: booking.StatusWaitlist,
	}
	h.bookings.byEvent[7] = append(h.bookings.byEvent[7], booking.Booking{
		ID: 201, EventID: 7, PlayerName: "Евгений Мёдов", UserID: int64Ptr(7),
		Status: booking.StatusWaitlist,
	})

	code, body := do(t, h.app, http.MethodPost, "/app/api/attend?vk_user_id=42&vk_chat_id=47", `{"event_id":7}`)
	if code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if !strings.Contains(body, `"status":"waitlist"`) || !strings.Contains(body, `"place":2`) {
		t.Errorf("answer: %s", body)
	}
}

func TestAttendRefusesCancelledAndPastGames(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*event.Event)
	}{
		{"cancelled", func(ev *event.Event) { ev.Status = event.StatusCancelled }},
		{"past", func(ev *event.Event) { ev.StartsAt = harnessNow().Add(-time.Hour) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			seedGame(h)
			ev := h.events.byID[7]
			tc.mutate(&ev)
			h.events.byID[7] = ev

			code, body := do(t, h.app, http.MethodPost,
				"/app/api/attend?vk_user_id=42&vk_chat_id=47", `{"event_id":7}`)
			if code != http.StatusConflict {
				t.Fatalf("status: %d (%s)", code, body)
			}
			if len(h.sync.booked) != 0 {
				t.Errorf("nothing must be booked: %+v", h.sync.booked)
			}
		})
	}
}

func TestAttendUnknownGameIsNotFound(t *testing.T) {
	h := newHarness(t, true)

	code, _ := do(t, h.app, http.MethodPost, "/app/api/attend?vk_user_id=42&vk_chat_id=47", `{"event_id":99}`)
	if code != http.StatusNotFound {
		t.Fatalf("status: %d, want 404", code)
	}
}

// Повторная запись не создаёт вторую: приложение может отправить запрос дважды.
func TestAttendRepeatIsRefused(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.sync.bookErr = booking.ErrAlreadyBooked

	code, body := do(t, h.app, http.MethodPost, "/app/api/attend?vk_user_id=42&vk_chat_id=47", `{"event_id":7}`)
	if code != http.StatusConflict {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if !strings.Contains(body, "уже записаны") {
		t.Errorf("answer: %s", body)
	}
}

func TestSkipCancelsAndReportsWhoTookTheSeat(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.sync.cancel = booking.CancelResult{
		Cancelled: booking.Booking{ID: 100, EventID: 7, PlayerName: "Евгений Мёдов", Status: booking.StatusCancelled},
		Promoted: &booking.Booking{
			ID: 102, EventID: 7, PlayerName: "Иван",
			Status: booking.StatusConfirmed, SeatNo: intPtr(1),
		},
	}

	code, body := do(t, h.app, http.MethodPost, "/app/api/skip?vk_user_id=42&vk_chat_id=47", `{"event_id":7}`)
	if code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if !strings.Contains(body, "1 - Иван") {
		t.Errorf("promotion must be visible: %s", body)
	}
	if h.sync.peerID != 2000000047 {
		t.Errorf("peer id: %d", h.sync.peerID)
	}
}

func TestSkipWithoutBookingExplains(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.sync.cancelErr = booking.ErrNotFound

	code, body := do(t, h.app, http.MethodPost, "/app/api/skip?vk_user_id=42&vk_chat_id=47", `{"event_id":7}`)
	if code != http.StatusConflict {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if !strings.Contains(body, "не записаны") {
		t.Errorf("answer: %s", body)
	}
}

// Без бота записывать некуда: строка в чат и анонс — его работа.
func TestBookingWithoutBotExplains(t *testing.T) {
	h := newHarnessWithoutBot(t)
	seedGame(h)

	code, body := do(t, h.app, http.MethodPost, "/app/api/attend?vk_user_id=42&vk_chat_id=47", `{"event_id":7}`)
	if code != http.StatusConflict {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if !strings.Contains(body, "бот не настроен") {
		t.Errorf("answer: %s", body)
	}
}

func TestBookingNeedsPostAndGameID(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)

	if code, _ := do(t, h.app, http.MethodGet, "/app/api/attend?vk_user_id=42&vk_chat_id=47", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", code)
	}
	if code, _ := do(t, h.app, http.MethodPost, "/app/api/attend?vk_user_id=42&vk_chat_id=47", `{}`); code != http.StatusBadRequest {
		t.Errorf("without event_id: status %d, want 400", code)
	}
}

// --- админские действия ---

// Администратор видит формы: флаг, предзаполнение из расписания и id записей,
// чтобы человека можно было снять.
func TestStateGivesTheAdminWhatTheFormsNeed(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.schedule.slots = []schedule.Slot{{ChatID: 1, Weekday: 0, Minutes: 10 * 60}}

	code, body := do(t, h.app, http.MethodGet, "/app/api/state?vk_user_id=42&vk_chat_id=47", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}

	var got struct {
		Admin bool `json:"admin"`
		Games []struct {
			Lineup []struct {
				BookingID int64 `json:"booking_id"`
			} `json:"lineup"`
		} `json:"games"`
		NextGame *struct {
			Date     string `json:"date"`
			Time     string `json:"time"`
			Title    string `json:"title"`
			Capacity int    `json:"capacity"`
		} `json:"next_game"`
		Schedule []struct {
			Weekday string `json:"weekday"`
			Time    string `json:"time"`
		} `json:"schedule"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("state json: %v (%q)", err, body)
	}

	if !got.Admin {
		t.Fatal("the administrator must see the admin flag")
	}
	// Расписание чата (вс 10:00) задаёт предзаполнение: ближайшее воскресенье.
	if got.NextGame == nil {
		t.Fatal("next_game must come to an administrator")
	}
	if got.NextGame.Date != "2026-09-27" || got.NextGame.Time != "10:00" {
		t.Fatalf("next_game: %+v", got.NextGame)
	}
	if got.NextGame.Title != "Волейбол" || got.NextGame.Capacity != 12 {
		t.Fatalf("defaults: %+v", got.NextGame)
	}
	if len(got.Schedule) != 1 || got.Schedule[0].Weekday != "вс" || got.Schedule[0].Time != "10:00" {
		t.Fatalf("schedule: %+v", got.Schedule)
	}
	if len(got.Games) != 1 || len(got.Games[0].Lineup) == 0 || got.Games[0].Lineup[0].BookingID == 0 {
		t.Fatalf("booking ids are needed to remove someone: %+v", got.Games)
	}
}

// Обычный участник не получает ни флага, ни предзаполнения форм.
func TestStateHidesAdminPayloadFromParticipants(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.chats.admin = false

	code, body := do(t, h.app, http.MethodGet, "/app/api/state?vk_user_id=42&vk_chat_id=47", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if strings.Contains(body, `"admin":true`) {
		t.Fatalf("participant must not be admin: %s", body)
	}
	if strings.Contains(body, "next_game") {
		t.Fatalf("no create form for a participant: %s", body)
	}
}

// «Создать игру» из приложения: дата и время читаются в зоне беседы, пустые
// название и вместимость берут умолчания бота.
func TestAdminCreatesGameFromTheApp(t *testing.T) {
	h := newHarness(t, true)

	code, body := do(t, h.app, http.MethodPost, "/app/api/game?vk_user_id=42&vk_chat_id=47",
		`{"date":"2026-09-27","time":"19:00","location":"СК «Спартак»"}`)
	if code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if len(h.sync.started) != 1 {
		t.Fatalf("the adapter must be asked to start the game: %+v", h.sync.started)
	}

	in := h.sync.started[0]
	if in.ChatID != 1 {
		t.Errorf("chat: %d", in.ChatID)
	}
	want := time.Date(2026, 9, 27, 19, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	if !in.StartsAt.Equal(want) {
		t.Errorf("starts_at: %s, want %s (зона беседы, не телефона)", in.StartsAt, want)
	}
	if in.Title != "Волейбол" || in.Capacity != 12 {
		t.Errorf("defaults: %+v", in)
	}
	if in.Location != "СК «Спартак»" {
		t.Errorf("location: %q", in.Location)
	}
	if h.sync.peerID != 2000000047 || h.sync.adminUser != 7 {
		t.Errorf("peer/user: %d/%d", h.sync.peerID, h.sync.adminUser)
	}
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, "27 сентября, 19:00") {
		t.Errorf("answer: %s", body)
	}
}

// Правка игры: то, что администратор задал в форме, доходит до адаптера.
func TestAdminEditsGameFromTheApp(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)

	code, body := do(t, h.app, http.MethodPost, "/app/api/game/edit?vk_user_id=42&vk_chat_id=47",
		`{"event_id":7,"date":"2026-09-28","time":"19:30","title":"Волейбол на траве","location":"СК «Спартак»","capacity":8}`)
	if code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if len(h.sync.edited) != 1 {
		t.Fatalf("edited: %+v", h.sync.edited)
	}

	in := h.sync.edited[0]
	if in.EventID != 7 || in.ChatID != 1 || in.Title != "Волейбол на траве" || in.Capacity != 8 {
		t.Fatalf("update input: %+v", in)
	}
	want := time.Date(2026, 9, 28, 19, 30, 0, 0, time.FixedZone("MSK", 3*60*60))
	if !in.StartsAt.Equal(want) {
		t.Errorf("starts_at: %s, want %s", in.StartsAt, want)
	}
}

// Кривая дата — 400, и до адаптера дело не доходит.
func TestAdminFormRefusesBadDate(t *testing.T) {
	h := newHarness(t, true)

	code, body := do(t, h.app, http.MethodPost, "/app/api/game?vk_user_id=42&vk_chat_id=47",
		`{"date":"27 сентября","time":"19:00"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if len(h.sync.started) != 0 {
		t.Fatalf("nothing must be created: %+v", h.sync.started)
	}
}

// Отмена игры и снятие человека из приложения.
func TestAdminCancelsGameAndRemovesParticipant(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)

	code, body := do(t, h.app, http.MethodPost, "/app/api/game/cancel?vk_user_id=42&vk_chat_id=47", `{"event_id":7}`)
	if code != http.StatusOK {
		t.Fatalf("cancel: status %d (%s)", code, body)
	}
	if len(h.sync.cancelled) != 1 || h.sync.cancelled[0] != 7 {
		t.Fatalf("cancelled: %+v", h.sync.cancelled)
	}

	seat := 1
	h.sync.removeRes = booking.CancelResult{
		Cancelled: booking.Booking{ID: 100, EventID: 7, PlayerName: "Пётр", SeatNo: &seat, Status: booking.StatusCancelled},
		Promoted:  &booking.Booking{ID: 102, EventID: 7, PlayerName: "Иван", SeatNo: &seat, Status: booking.StatusConfirmed},
	}
	code, body = do(t, h.app, http.MethodPost, "/app/api/remove?vk_user_id=42&vk_chat_id=47", `{"event_id":7,"booking_id":100}`)
	if code != http.StatusOK {
		t.Fatalf("remove: status %d (%s)", code, body)
	}
	if len(h.sync.removed) != 1 || h.sync.removed[0] != [2]int64{7, 100} {
		t.Fatalf("removed: %+v", h.sync.removed)
	}
	if !strings.Contains(body, "1 - Иван") {
		t.Errorf("the promotion must be visible: %s", body)
	}
}

// Участник не может ни создать игру, ни отменить её, ни снять человека: 403 на
// все ручки, и адаптер не трогают.
func TestAdminEndpointsAreRefusedToParticipants(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.chats.admin = false

	cases := []struct{ name, target, body string }{
		{"create", "/app/api/game", `{"date":"2026-09-27","time":"19:00"}`},
		{"edit", "/app/api/game/edit", `{"event_id":7,"date":"2026-09-27","time":"19:00"}`},
		{"cancel", "/app/api/game/cancel", `{"event_id":7}`},
		{"remove", "/app/api/remove", `{"event_id":7,"booking_id":100}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, h.app, http.MethodPost, tc.target+"?vk_user_id=42&vk_chat_id=47", tc.body)
			if code != http.StatusForbidden {
				t.Fatalf("status: %d (%s)", code, body)
			}
			if !strings.Contains(body, "администратор") {
				t.Errorf("answer: %s", body)
			}
		})
	}

	if len(h.sync.started)+len(h.sync.edited)+len(h.sync.cancelled)+len(h.sync.removed) != 0 {
		t.Fatalf("nothing must reach the adapter: %+v", h.sync)
	}
	if !strings.Contains(h.logs.String(), "admin action refused") {
		t.Errorf("the refusal must be logged: %q", h.logs.String())
	}
}

// Пока защищённый ключ не задан, админское действие разрешено, но след в логе
// остаётся: видно, что подпись не проверялась.
func TestAdminActionWithoutKeyIsLogged(t *testing.T) {
	h := newHarness(t, true)

	if code, body := do(t, h.app, http.MethodPost, "/app/api/game?vk_user_id=42&vk_chat_id=47",
		`{"date":"2026-09-27","time":"19:00"}`); code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if !strings.Contains(h.logs.String(), "without sign check") {
		t.Fatalf("log: %q", h.logs.String())
	}
}

// С настроенным ключом подпись проверяется, и это же действие проходит по
// официальной подписи из документации VK: наш собственный параметр (беседа в
// хеше) в подпись не входит.
func TestAdminActionWithKeyIsNotFlagged(t *testing.T) {
	h := newHarnessWithSecret(t, true, "wvl68m4dR1UpLrVRli")

	const signed = "vk_user_id=494075&vk_app_id=6736218&vk_is_app_user=1" +
		"&vk_are_notifications_enabled=1&vk_language=ru&vk_access_token_settings=" +
		"&vk_platform=android&hash=peer%3D2000000047" +
		"&sign=htQFduJpLxz7ribXRZpDFUH-XEUhC9rBPTJkjUFEkRA"

	code, body := do(t, h.app, http.MethodPost, "/app/api/game?"+signed, `{"date":"2026-09-27","time":"19:00"}`)
	if code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if len(h.sync.started) != 1 {
		t.Fatalf("started: %+v", h.sync.started)
	}
	if strings.Contains(h.logs.String(), "without sign check") {
		t.Fatalf("the sign was verified, nothing to flag: %q", h.logs.String())
	}
}

// --- приглашение человека ---

// Гостя может записать любой участник, а не только администратор: так в беседе
// и делают, просто написав «11 Сергей Иванов».
func TestParticipantInvitesGuestFromTheApp(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.chats.admin = false
	h.sync.bookRes = booking.Booking{
		ID: 300, EventID: 7, PlayerName: "Сергей Иванов",
		Status: booking.StatusConfirmed, SeatNo: intPtr(11),
	}

	code, body := do(t, h.app, http.MethodPost, "/app/api/guest?vk_user_id=42&vk_chat_id=47",
		`{"event_id":7,"name":"Сергей Иванов","seat_no":11}`)
	if code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if len(h.sync.booked) != 1 || h.sync.booked[0] != 7 {
		t.Fatalf("booked games: %+v", h.sync.booked)
	}
	if h.sync.bookGuest != "Сергей Иванов" || h.sync.bookSeat != 11 {
		t.Fatalf("guest/seat: %q/%d", h.sync.bookGuest, h.sync.bookSeat)
	}
	if h.sync.user.ID != 7 {
		t.Fatalf("the inviter must be the caller: %+v", h.sync.user)
	}
	for _, want := range []string{`"name":"Сергей Иванов"`, `"seat_no":11`} {
		if !strings.Contains(body, want) {
			t.Errorf("answer %s does not contain %s", body, want)
		}
	}
}

// Без номера места гостя записывают на любое свободное: seat 0 значит «какое
// найдётся».
func TestGuestInviteWithoutSeat(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.sync.bookRes = booking.Booking{
		ID: 301, EventID: 7, PlayerName: "Сергей", Status: booking.StatusConfirmed, SeatNo: intPtr(4),
	}

	code, body := do(t, h.app, http.MethodPost, "/app/api/guest?vk_user_id=42&vk_chat_id=47",
		`{"event_id":7,"name":"Сергей"}`)
	if code != http.StatusOK {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if h.sync.bookSeat != 0 {
		t.Fatalf("seat: %d, want 0 (любое свободное)", h.sync.bookSeat)
	}
	if !strings.Contains(body, `"seat_no":4`) {
		t.Errorf("answer: %s", body)
	}
}

func TestGuestInviteRefusesTakenSeat(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)
	h.sync.bookErr = booking.ErrSeatTaken

	code, body := do(t, h.app, http.MethodPost, "/app/api/guest?vk_user_id=42&vk_chat_id=47",
		`{"event_id":7,"name":"Сергей","seat_no":1}`)
	if code != http.StatusConflict {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if !strings.Contains(body, "занято") {
		t.Errorf("answer: %s", body)
	}
}

func TestGuestInviteNeedsAName(t *testing.T) {
	h := newHarness(t, true)
	seedGame(h)

	code, _ := do(t, h.app, http.MethodPost, "/app/api/guest?vk_user_id=42&vk_chat_id=47",
		`{"event_id":7,"name":"   "}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status: %d, want 400", code)
	}
	if len(h.sync.booked) != 0 {
		t.Fatalf("nothing must be booked: %+v", h.sync.booked)
	}
}

// Без бота гостя записать негде: строку в чат и анонс делает его адаптер.
func TestGuestInviteWithoutBotExplains(t *testing.T) {
	h := newHarnessWithoutBot(t)
	seedGame(h)

	code, body := do(t, h.app, http.MethodPost, "/app/api/guest?vk_user_id=42&vk_chat_id=47",
		`{"event_id":7,"name":"Сергей"}`)
	if code != http.StatusConflict {
		t.Fatalf("status: %d (%s)", code, body)
	}
	if !strings.Contains(body, "бот не настроен") {
		t.Errorf("answer: %s", body)
	}
}

func intPtr(v int) *int { return &v }

func int64Ptr(v int64) *int64 { return &v }
