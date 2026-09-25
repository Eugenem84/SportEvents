package booking_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"sportevents.local/internal/booking"
	"sportevents.local/internal/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCreateFreeSlotConfirms(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	userID := insertUser(t, ctx, pool, "Иван")
	eventID := insertEvent(t, ctx, pool, chatID, 2)

	b, err := svc.Create(ctx, booking.CreateInput{
		EventID:        eventID,
		PlayerName:     "Иван",
		UserID:         &userID,
		BookedByUserID: userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != booking.StatusConfirmed {
		t.Fatalf("status: %v", b.Status)
	}
}

func TestCreateLastSlotConfirms(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 1)
	admin := insertUser(t, ctx, pool, "Admin")

	b, err := svc.Create(ctx, booking.CreateInput{
		EventID:        eventID,
		PlayerName:     "Игрок 1",
		BookedByUserID: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != booking.StatusConfirmed {
		t.Fatalf("status: %v", b.Status)
	}
}

func TestCreateNoSlotsWaitlists(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 1)
	admin := insertUser(t, ctx, pool, "Admin")

	first, err := svc.Create(ctx, booking.CreateInput{
		EventID:        eventID,
		PlayerName:     "Первый",
		BookedByUserID: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != booking.StatusConfirmed {
		t.Fatalf("first status: %v", first.Status)
	}

	second, err := svc.Create(ctx, booking.CreateInput{
		EventID:        eventID,
		PlayerName:     "Второй",
		BookedByUserID: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != booking.StatusWaitlist {
		t.Fatalf("second status: %v", second.Status)
	}
}

func TestCreateDuplicateActiveUser(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 5)
	userID := insertUser(t, ctx, pool, "Иван")

	_, err := svc.Create(ctx, booking.CreateInput{
		EventID:        eventID,
		PlayerName:     "Иван",
		UserID:         &userID,
		BookedByUserID: userID,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Create(ctx, booking.CreateInput{
		EventID:        eventID,
		PlayerName:     "Иван",
		UserID:         &userID,
		BookedByUserID: userID,
	})
	if !errors.Is(err, booking.ErrAlreadyBooked) {
		t.Fatalf("want ErrAlreadyBooked, got %v", err)
	}
}

func TestCreateTwoGuestsSameNameAllowed(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 5)
	admin := insertUser(t, ctx, pool, "Admin")

	for i := 0; i < 2; i++ {
		_, err := svc.Create(ctx, booking.CreateInput{
			EventID:        eventID,
			PlayerName:     "Гость Вася",
			BookedByUserID: admin,
		})
		if err != nil {
			t.Fatalf("guest %d: %v", i, err)
		}
	}

	list, err := svc.ListByEvent(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 guest bookings, got %d", len(list))
	}
}

// --- Helpers (integration tests need a live PostgreSQL) ---

func setup(t *testing.T) (context.Context, *booking.Service, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5434/booking?sslmode=disable"
	}
	pool, err := postgres.NewPool(ctx, url)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return ctx, booking.NewService(pool), pool
}

func insertChat(t *testing.T, ctx context.Context, pool *pgxpool.Pool, title string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO chats (title) VALUES ($1) RETURNING id`, title).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO users (display_name) VALUES ($1) RETURNING id`, name).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, chatID int64, capacity int) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO events (chat_id, starts_at, title, capacity)
		VALUES ($1, now() + interval '1 day', 'Игра', $2)
		RETURNING id`,
		chatID, capacity,
	).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// mustCreate books a guest (no user_id) so FIFO tests only depend on
// created_at/id ordering.
func mustCreate(t *testing.T, svc *booking.Service, ctx context.Context, eventID, bookedBy int64, name string) booking.Booking {
	t.Helper()
	b, err := svc.Create(ctx, booking.CreateInput{
		EventID:        eventID,
		PlayerName:     name,
		BookedByUserID: bookedBy,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- Cancel / promotion ---

// Отмена игры снимает и записи: «записанным» на отменённую игру оставаться
// нельзя, а «мои записи» больше её не показывают.
func TestCancelAllForEvent(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "Волейбол")
	ev := insertEvent(t, ctx, pool, chatID, 4)
	player := insertUser(t, ctx, pool, "Пётр")
	organizer := insertUser(t, ctx, pool, "Организатор")

	// Трое гостей в составе и одна именная запись — она же четвёртое место.
	mustCreate(t, svc, ctx, ev, organizer, "гость 1")
	mustCreate(t, svc, ctx, ev, organizer, "гость 2")
	mustCreate(t, svc, ctx, ev, organizer, "гость 3")
	if _, err := svc.Create(ctx, booking.CreateInput{
		EventID: ev, PlayerName: "Пётр", UserID: &player, BookedByUserID: player,
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := svc.CancelAllForEvent(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 4 {
		t.Fatalf("removed: want 4, got %d", removed)
	}

	active, err := svc.ListByEvent(ctx, ev, booking.StatusConfirmed, booking.StatusWaitlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("no active bookings must remain: %+v", active)
	}
	mine, err := svc.ListActiveByUser(ctx, player, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 0 {
		t.Fatalf("a cancelled game must leave «мои записи»: %+v", mine)
	}

	// Повторный вызов ничего не снимает и не падает.
	again, err := svc.CancelAllForEvent(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("second cancel: want 0, got %d", again)
	}
}

func TestCancelPromotesFIFO(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 1)
	admin := insertUser(t, ctx, pool, "Admin")

	first := mustCreate(t, svc, ctx, eventID, admin, "Первый")
	second := mustCreate(t, svc, ctx, eventID, admin, "Второй")
	third := mustCreate(t, svc, ctx, eventID, admin, "Третий")

	if first.Status != booking.StatusConfirmed {
		t.Fatalf("first: want confirmed, got %v", first.Status)
	}
	if second.Status != booking.StatusWaitlist || third.Status != booking.StatusWaitlist {
		t.Fatalf("second/third: want waitlist, got %v / %v", second.Status, third.Status)
	}

	res, err := svc.Cancel(ctx, eventID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted == nil {
		t.Fatal("want promotion after cancelling confirmed booking, got none")
	}
	if res.Promoted.ID != second.ID {
		t.Fatalf("FIFO: want promoted %d, got %d", second.ID, res.Promoted.ID)
	}
	if res.Promoted.Status != booking.StatusConfirmed {
		t.Fatalf("promoted status: want confirmed, got %v", res.Promoted.Status)
	}

	cancelled, err := svc.Get(ctx, eventID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != booking.StatusCancelled {
		t.Fatalf("cancelled status: %v", cancelled.Status)
	}

	gotSecond, err := svc.Get(ctx, eventID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSecond.Status != booking.StatusConfirmed {
		t.Fatalf("second in db: want confirmed, got %v", gotSecond.Status)
	}
	gotThird, err := svc.Get(ctx, eventID, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotThird.Status != booking.StatusWaitlist {
		t.Fatalf("third still waitlist: got %v", gotThird.Status)
	}
}

func TestCancelWaitlistDoesNotPromote(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 1)
	admin := insertUser(t, ctx, pool, "Admin")

	first := mustCreate(t, svc, ctx, eventID, admin, "Первый")
	second := mustCreate(t, svc, ctx, eventID, admin, "Второй") // waitlist

	res, err := svc.Cancel(ctx, eventID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted != nil {
		t.Fatalf("cancelling a waitlist must not promote, got %+v", res.Promoted)
	}
	confirmed, err := svc.ListByEvent(ctx, eventID, booking.StatusConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmed) != 1 || confirmed[0].ID != first.ID {
		t.Fatalf("confirmed after waitlist cancel: %+v", confirmed)
	}
}

// --- Concurrency ---

func TestConcurrentBookingCapacityNotExceeded(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	const capacity, total = 3, 10
	eventID := insertEvent(t, ctx, pool, chatID, capacity)

	userIDs := make([]int64, total)
	for i := range userIDs {
		userIDs[i] = insertUser(t, ctx, pool, fmt.Sprintf("Игрок %d", i))
	}

	errs := make(chan error, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(id int64, name string) {
			defer wg.Done()
			_, err := svc.Create(ctx, booking.CreateInput{
				EventID:        eventID,
				PlayerName:     name,
				UserID:         &id,
				BookedByUserID: id,
			})
			if err != nil {
				errs <- err
			}
		}(userIDs[i], fmt.Sprintf("Игрок %d", i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent create: %v", err)
	}

	confirmed, err := svc.ListByEvent(ctx, eventID, booking.StatusConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	waitlist, err := svc.ListByEvent(ctx, eventID, booking.StatusWaitlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmed) != capacity {
		t.Fatalf("confirmed: want %d, got %d", capacity, len(confirmed))
	}
	if len(waitlist) != total-capacity {
		t.Fatalf("waitlist: want %d, got %d", total-capacity, len(waitlist))
	}
}

// --- Notification after promotion (no silent guest promotion) ---

type recordingNotifier struct {
	mu    sync.Mutex
	calls []booking.Booking
}

func (r *recordingNotifier) NotifyPromotion(_ context.Context, b booking.Booking) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, b)
	return nil
}

func (r *recordingNotifier) snapshot() []booking.Booking {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]booking.Booking(nil), r.calls...)
}

func TestNotifyPromotionNotSilentForGuest(t *testing.T) {
	ctx, _, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 1)
	admin := insertUser(t, ctx, pool, "Admin")

	notifier := &recordingNotifier{}
	svc := booking.NewService(pool, notifier)

	first := mustCreate(t, svc, ctx, eventID, admin, "Гость Вася")  // confirmed
	second := mustCreate(t, svc, ctx, eventID, admin, "Гость Пётр") // waitlist

	res, err := svc.Cancel(ctx, eventID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted == nil {
		t.Fatal("want promotion")
	}
	if res.Promoted.ID != second.ID {
		t.Fatalf("promoted: want %d, got %d", second.ID, res.Promoted.ID)
	}
	if res.Promoted.UserID != nil {
		t.Fatalf("promoted guest must have nil user_id, got %v", res.Promoted.UserID)
	}

	calls := notifier.snapshot()
	if len(calls) != 1 {
		t.Fatalf("notifier: want exactly 1 call, got %d", len(calls))
	}
	if calls[0].ID != second.ID || calls[0].UserID != nil || calls[0].BookedByUserID != admin {
		t.Fatalf("notified booking: %+v", calls[0])
	}

	// Cancelling a waitlist booking must not trigger notification.
	third := mustCreate(t, svc, ctx, eventID, admin, "Гость Семён") // waitlist
	if _, err := svc.Cancel(ctx, eventID, third.ID); err != nil {
		t.Fatal(err)
	}
	if got := notifier.snapshot(); len(got) != 1 {
		t.Fatalf("notifier: cancelling waitlist must not notify, got %d calls", len(got))
	}
}

// --- ListActiveByUser ("мои записи") ---

func TestListActiveByUser(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	userID := insertUser(t, ctx, pool, "Иван")
	other := insertUser(t, ctx, pool, "Пётр")

	full := insertEvent(t, ctx, pool, chatID, 1)
	roomy := insertEvent(t, ctx, pool, chatID, 5)

	mine := mustBookUser(t, svc, ctx, roomy, userID, "Иван")        // confirmed
	_ = mustBookUser(t, svc, ctx, roomy, other, "Пётр")             // чужая запись
	_ = mustBookUser(t, svc, ctx, full, other, "Пётр")              // занимает единственное место
	queued := mustBookUser(t, svc, ctx, full, userID, "Иван")       // waitlist
	_ = mustCreate(t, svc, ctx, roomy, other, "Гость без identity") // гость

	got, err := svc.ListActiveByUser(ctx, userID, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 active bookings, got %d: %+v", len(got), got)
	}

	byID := map[int64]booking.BookingWithEvent{}
	for _, b := range got {
		byID[b.ID] = b
	}
	if b, ok := byID[mine.ID]; !ok || b.Status != booking.StatusConfirmed {
		t.Fatalf("confirmed booking missing: %+v", b)
	}
	if b, ok := byID[queued.ID]; !ok || b.Status != booking.StatusWaitlist {
		t.Fatalf("waitlist booking missing: %+v", b)
	}
	if b := byID[mine.ID]; b.EventID != roomy || b.EventTitle == "" || b.EventCapacity != 5 {
		t.Fatalf("event fields not filled: %+v", b)
	}
}

func TestListActiveByUserIgnoresCancelledAndPast(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	userID := insertUser(t, ctx, pool, "Иван")
	eventID := insertEvent(t, ctx, pool, chatID, 5)

	b := mustBookUser(t, svc, ctx, eventID, userID, "Иван")
	if _, err := svc.Cancel(ctx, eventID, b.ID); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ListActiveByUser(ctx, userID, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("cancelled booking must not be listed: %+v", got)
	}

	// Игра началась раньше "from" — на будущее её уже не показываем.
	got, err = svc.ListActiveByUser(ctx, userID, time.Now().UTC().Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("events before 'from' must not be listed: %+v", got)
	}
}

// mustBookUser books a player who has an identity (not a guest).
func mustBookUser(t *testing.T, svc *booking.Service, ctx context.Context, eventID, userID int64, name string) booking.Booking {
	t.Helper()
	b, err := svc.Create(ctx, booking.CreateInput{
		EventID:        eventID,
		PlayerName:     name,
		UserID:         &userID,
		BookedByUserID: userID,
	})
	if err != nil {
		t.Fatalf("book %s: %v", name, err)
	}
	return b
}

// --- Номера мест ---

func TestCreateAssignsLowestFreeSeat(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 3)
	admin := insertUser(t, ctx, pool, "Admin")

	for i, want := range []int{1, 2, 3} {
		b := mustCreate(t, svc, ctx, eventID, admin, fmt.Sprintf("Гость %d", i))
		if b.SeatNo == nil || *b.SeatNo != want {
			t.Fatalf("booking %d: seat %v, want %d", i, b.SeatNo, want)
		}
	}

	extra := mustCreate(t, svc, ctx, eventID, admin, "Гость 4")
	if extra.Status != booking.StatusWaitlist {
		t.Fatalf("status: %v", extra.Status)
	}
	if extra.SeatNo != nil {
		t.Fatalf("a booking in reserve must have no seat, got %v", *extra.SeatNo)
	}
}

// Освободившееся место не сдвигает нумерацию: следующий записавшийся занимает
// именно освободившийся номер.
func TestFreedSeatIsReusedByNextBooking(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 3)
	admin := insertUser(t, ctx, pool, "Admin")

	_ = mustCreate(t, svc, ctx, eventID, admin, "Первый")       // место 1
	second := mustCreate(t, svc, ctx, eventID, admin, "Второй") // место 2
	third := mustCreate(t, svc, ctx, eventID, admin, "Третий")  // место 3
	if third.SeatNo == nil || *third.SeatNo != 3 {
		t.Fatalf("third seat: %v", third.SeatNo)
	}

	if _, err := svc.Cancel(ctx, eventID, second.ID); err != nil {
		t.Fatal(err)
	}

	next := mustCreate(t, svc, ctx, eventID, admin, "Четвёртый")
	if next.SeatNo == nil || *next.SeatNo != 2 {
		t.Fatalf("the freed seat 2 must be reused, got %v", next.SeatNo)
	}
}

// Поднявшийся из резерва занимает именно освободившееся место.
func TestPromotionTakesFreedSeat(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 2)
	admin := insertUser(t, ctx, pool, "Admin")

	first := mustCreate(t, svc, ctx, eventID, admin, "Первый") // место 1
	_ = mustCreate(t, svc, ctx, eventID, admin, "Второй")      // место 2
	queued := mustCreate(t, svc, ctx, eventID, admin, "Резерв")

	res, err := svc.Cancel(ctx, eventID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted == nil {
		t.Fatal("want a promotion")
	}
	if res.Promoted.ID != queued.ID {
		t.Fatalf("promoted: want %d, got %d", queued.ID, res.Promoted.ID)
	}
	if res.Promoted.SeatNo == nil || *res.Promoted.SeatNo != 1 {
		t.Fatalf("promoted seat: want 1, got %v", res.Promoted.SeatNo)
	}

	stored, err := svc.Get(ctx, eventID, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != booking.StatusConfirmed || stored.SeatNo == nil || *stored.SeatNo != 1 {
		t.Fatalf("stored booking: %+v", stored)
	}
}

// Одно место — один человек, и номер у резерва никогда не появляется: проверяем
// на параллельной записи, где гонки реальны.
func TestConcurrentBookingsHaveUniqueSeats(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	eventID := insertEvent(t, ctx, pool, chatID, 4)
	admin := insertUser(t, ctx, pool, "Admin")
	const total = 8

	var wg sync.WaitGroup
	var mu sync.Mutex
	seats := map[int]bool{}
	confirmed := 0
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, err := svc.Create(ctx, booking.CreateInput{
				EventID:        eventID,
				PlayerName:     fmt.Sprintf("Игрок %d", i),
				BookedByUserID: admin,
			})
			if err != nil {
				t.Errorf("create %d: %v", i, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case b.Status == booking.StatusConfirmed && b.SeatNo == nil:
				t.Errorf("confirmed booking without a seat: %+v", b)
			case b.Status == booking.StatusWaitlist && b.SeatNo != nil:
				t.Errorf("reserve booking must have no seat: %+v", b)
			case b.Status == booking.StatusConfirmed:
				confirmed++
				if seats[*b.SeatNo] {
					t.Errorf("seat %d taken twice", *b.SeatNo)
				}
				seats[*b.SeatNo] = true
			}
		}(i)
	}
	wg.Wait()

	if confirmed != 4 {
		t.Fatalf("confirmed: want 4, got %d", confirmed)
	}
	for seat := 1; seat <= 4; seat++ {
		if !seats[seat] {
			t.Fatalf("seat %d must be taken, got %v", seat, seats)
		}
	}
}
