package announce_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"sportevents.local/internal/announce"
	"sportevents.local/internal/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSaveAndGet(t *testing.T) {
	ctx, svc, pool := setup(t)
	eventID := insertEvent(t, ctx, pool)

	ref := announce.Ref{
		EventID:        eventID,
		Platform:       "vk",
		ExternalChatID: "2000000002",
		MessageID:      501,
	}
	if err := svc.Save(ctx, ref); err != nil {
		t.Fatal(err)
	}

	got, ok, err := svc.Get(ctx, eventID, "vk")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("want saved announcement")
	}
	if got != ref {
		t.Fatalf("got %+v, want %+v", got, ref)
	}
}

// Announcing the same event twice (a re-post or a resend after an error) must
// replace the reference, not fail: the last message is the one to edit.
func TestSaveReplacesExisting(t *testing.T) {
	ctx, svc, pool := setup(t)
	eventID := insertEvent(t, ctx, pool)

	if err := svc.Save(ctx, announce.Ref{EventID: eventID, Platform: "vk", ExternalChatID: "2000000002", MessageID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Save(ctx, announce.Ref{EventID: eventID, Platform: "vk", ExternalChatID: "2000000002", MessageID: 2}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := svc.Get(ctx, eventID, "vk")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.MessageID != 2 {
		t.Fatalf("want the last message id, got %+v (ok=%v)", got, ok)
	}
}

// Нулевой MessageID — это «анонс есть, id пока неизвестен»: в беседе VK
// отвечает 0 на messages.send, и id приходит с первым нажатием на сам анонс.
func TestSaveUnknownMessageID(t *testing.T) {
	ctx, svc, pool := setup(t)
	eventID := insertEvent(t, ctx, pool)

	if err := svc.Save(ctx, announce.Ref{EventID: eventID, Platform: "vk", ExternalChatID: "2000000002"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := svc.Get(ctx, eventID, "vk")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.MessageID != 0 {
		t.Fatalf("unknown id must stay zero, got %d", got.MessageID)
	}

	// Нажатие на кнопку анонса доносит id — и он заменяет неизвестный.
	if err := svc.Save(ctx, announce.Ref{EventID: eventID, Platform: "vk", ExternalChatID: "2000000002", MessageID: 85}); err != nil {
		t.Fatal(err)
	}
	got, _, err = svc.Get(ctx, eventID, "vk")
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageID != 85 {
		t.Fatalf("want the learned id, got %d", got.MessageID)
	}
}

func TestGetUnknownPlatform(t *testing.T) {
	ctx, svc, pool := setup(t)
	eventID := insertEvent(t, ctx, pool)

	if err := svc.Save(ctx, announce.Ref{EventID: eventID, Platform: "vk", ExternalChatID: "1", MessageID: 1}); err != nil {
		t.Fatal(err)
	}

	_, ok, err := svc.Get(ctx, eventID, "max")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("announcement of another platform must not be returned")
	}
}

func TestSaveInvalid(t *testing.T) {
	_, svc, _ := setup(t)
	valid := announce.Ref{EventID: 1, Platform: "vk", ExternalChatID: "10", MessageID: 5}

	cases := map[string]announce.Ref{
		"no event":    {Platform: "vk", ExternalChatID: "10", MessageID: 5},
		"no platform": {EventID: 1, ExternalChatID: "10", MessageID: 5},
		"no channel":  {EventID: 1, Platform: "vk", MessageID: 5},
		"bad message": {EventID: 1, Platform: "vk", ExternalChatID: "10", MessageID: -1},
	}
	for name, ref := range cases {
		if err := svc.Save(context.Background(), ref); !errors.Is(err, announce.ErrInvalid) {
			t.Fatalf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if err := svc.Save(context.Background(), valid); err != nil {
		t.Fatalf("valid ref must be accepted, got %v", err)
	}
}

func setup(t *testing.T) (context.Context, *announce.Service, *pgxpool.Pool) {
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
	return ctx, announce.NewService(pool), pool
}

// insertEvent creates the minimum chain the announcement references:
// chat -> event.
func insertEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var chatID int64
	if err := pool.QueryRow(ctx, `INSERT INTO chats (title) VALUES ('A') RETURNING id`).Scan(&chatID); err != nil {
		t.Fatal(err)
	}
	var eventID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO events (chat_id, starts_at, title, capacity)
		VALUES ($1, now() + interval '1 day', 'Волейбол', 12)
		RETURNING id`,
		chatID,
	).Scan(&eventID)
	if err != nil {
		t.Fatal(err)
	}
	return eventID
}
