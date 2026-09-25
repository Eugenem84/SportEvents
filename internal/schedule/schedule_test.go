package schedule_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"sportevents.local/internal/postgres"
	"sportevents.local/internal/schedule"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSetListDelete(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool)

	if _, err := svc.Set(ctx, chatID, 0, 10*60); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Set(ctx, chatID, 3, 19*60); err != nil {
		t.Fatal(err)
	}

	got, err := svc.List(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Weekday != 0 || got[1].Weekday != 3 {
		t.Fatalf("list must be ordered by weekday: %+v", got)
	}
	if got[0].At() != "10:00" {
		t.Fatalf("slot time: %q", got[0].At())
	}
}

// One weekday keeps one time in V1: setting it again replaces the time
// instead of adding a second slot.
func TestSetReplacesSameWeekday(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool)

	if _, err := svc.Set(ctx, chatID, 0, 10*60); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Set(ctx, chatID, 0, 11*60+30); err != nil {
		t.Fatal(err)
	}

	got, err := svc.List(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Minutes != 11*60+30 {
		t.Fatalf("replace: %+v", got)
	}
}

func TestDeleteReportsWhatHappened(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool)
	if _, err := svc.Set(ctx, chatID, 6, 12*60); err != nil {
		t.Fatal(err)
	}

	ok, err := svc.Delete(ctx, chatID, 6)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("want the slot removed")
	}

	ok, err = svc.Delete(ctx, chatID, 6)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("deleting a weekday that is not scheduled must report false")
	}
}

func TestListEmpty(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool)

	got, err := svc.List(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want an empty schedule, got %+v", got)
	}
}

func TestSetInvalid(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool)

	cases := []struct {
		name             string
		chatID           int64
		weekday, minutes int
	}{
		{"no chat", 0, 0, 600},
		{"weekday too large", chatID, 7, 600},
		{"negative weekday", chatID, -1, 600},
		{"minutes into next day", chatID, 0, 24 * 60},
		{"negative minutes", chatID, 0, -1},
	}
	for _, tc := range cases {
		if _, err := svc.Set(ctx, tc.chatID, tc.weekday, tc.minutes); !errors.Is(err, schedule.ErrInvalid) {
			t.Fatalf("%s: want ErrInvalid, got %v", tc.name, err)
		}
	}
}

func TestSetUnknownChat(t *testing.T) {
	ctx, svc, _ := setup(t)
	if _, err := svc.Set(ctx, 9_000_000_001, 0, 600); !errors.Is(err, schedule.ErrChatNotFound) {
		t.Fatalf("want ErrChatNotFound, got %v", err)
	}
}

func setup(t *testing.T) (context.Context, *schedule.Service, *pgxpool.Pool) {
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
	return ctx, schedule.NewService(pool), pool
}

func insertChat(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO chats (title) VALUES ('Расписание') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
