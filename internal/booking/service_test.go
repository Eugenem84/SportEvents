package booking_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

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
