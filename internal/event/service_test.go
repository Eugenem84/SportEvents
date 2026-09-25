package event_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"sportevents.local/internal/event"
	"sportevents.local/internal/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCreateAndGet(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "Волейбол Иваново")
	starts := time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC)

	created, err := svc.Create(ctx, event.CreateInput{
		ChatID:   chatID,
		StartsAt: starts,
		Title:    "Волейбол 2 сентября",
		Location: "Зал 1",
		Capacity: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.ChatID != chatID {
		t.Fatalf("unexpected event: %+v", created)
	}
	if !created.StartsAt.Equal(starts) {
		t.Fatalf("starts_at: got %v want %v", created.StartsAt, starts)
	}

	got, err := svc.Get(ctx, chatID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != created.Title || got.Capacity != 12 || got.Location != "Зал 1" {
		t.Fatalf("get mismatch: %+v", got)
	}
}

func TestGetBelongsToChat(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatA := insertChat(t, ctx, pool, "A")
	chatB := insertChat(t, ctx, pool, "B")

	created, err := svc.Create(ctx, event.CreateInput{
		ChatID:   chatA,
		StartsAt: time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC),
		Title:    "Игра A",
		Capacity: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Get(ctx, chatB, created.ID)
	if !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCreateUnknownChat(t *testing.T) {
	ctx, svc, _ := setup(t)
	_, err := svc.Create(ctx, event.CreateInput{
		ChatID:   9_000_000_001,
		StartsAt: time.Now().UTC(),
		Title:    "Нет такого чата",
		Capacity: 4,
	})
	if !errors.Is(err, event.ErrChatNotFound) {
		t.Fatalf("want ErrChatNotFound, got %v", err)
	}
}

func TestCreateInvalid(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "C")
	valid := event.CreateInput{
		ChatID:   chatID,
		StartsAt: time.Now().UTC(),
		Title:    "Игра",
		Capacity: 4,
	}

	cases := []event.CreateInput{
		func() event.CreateInput { in := valid; in.ChatID = 0; return in }(),
		func() event.CreateInput { in := valid; in.StartsAt = time.Time{}; return in }(),
		func() event.CreateInput { in := valid; in.Title = "  "; return in }(),
		func() event.CreateInput { in := valid; in.Capacity = 0; return in }(),
	}
	for _, in := range cases {
		if _, err := svc.Create(ctx, in); !errors.Is(err, event.ErrInvalid) {
			t.Fatalf("input %+v: want ErrInvalid, got %v", in, err)
		}
	}
}

func TestListUpcoming(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatA := insertChat(t, ctx, pool, "A")
	chatB := insertChat(t, ctx, pool, "B")
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	mustCreate(t, svc, ctx, event.CreateInput{ChatID: chatA, StartsAt: now.Add(-time.Hour), Title: "прошло", Capacity: 2})
	first := mustCreate(t, svc, ctx, event.CreateInput{ChatID: chatA, StartsAt: now.Add(time.Hour), Title: "скоро", Capacity: 2})
	second := mustCreate(t, svc, ctx, event.CreateInput{ChatID: chatA, StartsAt: now.Add(2 * time.Hour), Title: "позже", Capacity: 2})
	mustCreate(t, svc, ctx, event.CreateInput{ChatID: chatB, StartsAt: now.Add(time.Hour), Title: "чужой чат", Capacity: 2})

	list, err := svc.ListUpcoming(ctx, chatA, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("list: %+v", list)
	}

	limited, err := svc.ListUpcoming(ctx, chatA, now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].ID != first.ID {
		t.Fatalf("limit: %+v", limited)
	}
}

func TestSetCapacity(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	ev := mustCreate(t, svc, ctx, event.CreateInput{
		ChatID:   chatID,
		StartsAt: time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC),
		Title:    "Игра",
		Capacity: 4,
	})

	if err := svc.SetCapacity(ctx, chatID, ev.ID, 6); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, chatID, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Capacity != 6 {
		t.Fatalf("capacity=%d", got.Capacity)
	}

	userID := insertUser(t, ctx, pool, "Иван")
	insertConfirmed(t, ctx, pool, ev.ID, userID, userID)
	user2 := insertUser(t, ctx, pool, "Пётр")
	insertConfirmed(t, ctx, pool, ev.ID, user2, user2)

	if err := svc.SetCapacity(ctx, chatID, ev.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetCapacity(ctx, chatID, ev.ID, 1); !errors.Is(err, event.ErrCapacityBelowConfirmed) {
		t.Fatalf("want ErrCapacityBelowConfirmed, got %v", err)
	}

	otherChat := insertChat(t, ctx, pool, "B")
	if err := svc.SetCapacity(ctx, otherChat, ev.ID, 10); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("want ErrNotFound for other chat, got %v", err)
	}
}

// Отмена игры: статус меняется, игра пропадает из предстоящих, а повторная
// отмена честно сообщает, что игра уже отменена.
func TestCancelEvent(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "Волейбол")
	ev := mustCreate(t, svc, ctx, event.CreateInput{
		ChatID:   chatID,
		StartsAt: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC),
		Title:    "Волейбол",
		Capacity: 12,
	})
	if ev.Status != event.StatusScheduled {
		t.Fatalf("a new game must be scheduled, got %q", ev.Status)
	}

	cancelled, err := svc.Cancel(ctx, chatID, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != event.StatusCancelled {
		t.Fatalf("status: %q", cancelled.Status)
	}

	got, err := svc.Get(ctx, chatID, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != event.StatusCancelled {
		t.Fatalf("the game must stay cancelled: %+v", got)
	}

	upcoming, err := svc.ListUpcomingWithCounts(ctx, chatID, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(upcoming) != 0 {
		t.Fatalf("a cancelled game is not upcoming: %+v", upcoming)
	}

	if _, err := svc.Cancel(ctx, chatID, ev.ID); !errors.Is(err, event.ErrAlreadyCancelled) {
		t.Fatalf("want ErrAlreadyCancelled, got %v", err)
	}
	if _, err := svc.Cancel(ctx, chatID+9999, ev.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("want ErrNotFound for another chat, got %v", err)
	}
}

func setup(t *testing.T) (context.Context, *event.Service, *pgxpool.Pool) {
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
	return ctx, event.NewService(pool), pool
}

func mustCreate(t *testing.T, svc *event.Service, ctx context.Context, in event.CreateInput) event.Event {
	t.Helper()
	e, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return e
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

func insertConfirmed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID, userID, bookedBy int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO bookings (event_id, player_name, user_id, booked_by_user_id, status)
		VALUES ($1, $2, $3, $4, 'confirmed')`,
		eventID, "игрок", userID, bookedBy,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// --- Counters for the chat list ("свободных мест") ---

func TestListUpcomingWithCounts(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	full := mustCreate(t, svc, ctx, event.CreateInput{
		ChatID: chatID, StartsAt: now.Add(time.Hour), Title: "Заполнена", Capacity: 2,
	})
	roomy := mustCreate(t, svc, ctx, event.CreateInput{
		ChatID: chatID, StartsAt: now.Add(2 * time.Hour), Title: "Есть места", Capacity: 5,
	})

	u1 := insertUser(t, ctx, pool, "Иван")
	u2 := insertUser(t, ctx, pool, "Пётр")
	u3 := insertUser(t, ctx, pool, "Сергей")
	insertConfirmed(t, ctx, pool, full.ID, u1, u1)
	insertConfirmed(t, ctx, pool, full.ID, u2, u2)
	insertWaitlist(t, ctx, pool, full.ID, u3, u3)
	insertConfirmed(t, ctx, pool, roomy.ID, u1, u1)

	list, err := svc.ListUpcomingWithCounts(ctx, chatID, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(list), list)
	}
	if list[0].ID != full.ID || list[0].Confirmed != 2 || list[0].Waitlist != 1 || list[0].Free != 0 {
		t.Fatalf("full event: %+v", list[0])
	}
	if list[1].ID != roomy.ID || list[1].Confirmed != 1 || list[1].Waitlist != 0 || list[1].Free != 4 {
		t.Fatalf("roomy event: %+v", list[1])
	}
}

// Free is capacity - confirmed and must never go negative, whatever the data.
func TestListUpcomingWithCountsNeverNegativeFree(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "A")
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	ev := mustCreate(t, svc, ctx, event.CreateInput{
		ChatID: chatID, StartsAt: now.Add(time.Hour), Title: "Игра", Capacity: 2,
	})
	for _, name := range []string{"Иван", "Пётр", "Сергей"} {
		u := insertUser(t, ctx, pool, name)
		insertConfirmed(t, ctx, pool, ev.ID, u, u)
	}

	list, err := svc.ListUpcomingWithCounts(ctx, chatID, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 event, got %d", len(list))
	}
	if list[0].Free != 0 {
		t.Fatalf("free must not be negative, got %d", list[0].Free)
	}
	if list[0].Confirmed != 3 {
		t.Fatalf("confirmed=%d", list[0].Confirmed)
	}
}

func insertWaitlist(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID, userID, bookedBy int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO bookings (event_id, player_name, user_id, booked_by_user_id, status)
		VALUES ($1, $2, $3, $4, 'waitlist')`,
		eventID, "игрок", userID, bookedBy,
	)
	if err != nil {
		t.Fatal(err)
	}
}
