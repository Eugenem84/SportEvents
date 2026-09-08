package chat_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"sportevents.local/internal/chat"
	"sportevents.local/internal/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Unique per-run external ids keep these integration tests repeatable
// against ashared PostgreSQL: leftover rows from earlier runs cannot
// collide with the unique indexes of chat_channels / user_identities.

func TestFindByChannelLinked(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatID := insertChat(t, ctx, pool, "Волейбол")
	extID := uniqueExt(t, "chat")
	insertChannel(t, ctx, pool, chatID, chat.PlatformVK, extID)

	found, err := svc.FindByChannel(ctx, chat.PlatformVK, extID)
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("want linked chat, got nil")
	}
	if found.ID != chatID || found.Title != "Волейбол" {
		t.Fatalf("chat mismatch: %+v", found)
	}
}

func TestFindByChannelNotConnectedReturnsNil(t *testing.T) {
	ctx, svc, pool := setup(t)
	_ = insertChat(t, ctx, pool, "A") // chat exists, but no channel

	found, err := svc.FindByChannel(ctx, chat.PlatformVK, uniqueExt(t, "none"))
	if err != nil {
		t.Fatal(err)
	}
	if found != nil {
		t.Fatalf("want nil for unconnected channel, got %+v", found)
	}
}

func TestFindByChannelScopedToChat(t *testing.T) {
	ctx, svc, pool := setup(t)
	chatA := insertChat(t, ctx, pool, "A")
	_ = insertChat(t, ctx, pool, "B")
	extID := uniqueExt(t, "chat")
	insertChannel(t, ctx, pool, chatA, chat.PlatformVK, extID)

	found, err := svc.FindByChannel(ctx, chat.PlatformVK, extID)
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || found.ID != chatA {
		t.Fatalf("want chat A, got %+v", found)
	}
}

func TestFindOrCreateUserByIdempotent(t *testing.T) {
	ctx, svc, pool := setup(t)
	extID := uniqueExt(t, "user")

	u1, err := svc.FindOrCreateUserByExternalID(ctx, chat.PlatformVK, extID, "Иван Петров")
	if err != nil {
		t.Fatal(err)
	}
	if u1.ID == 0 || u1.DisplayName != "Иван Петров" {
		t.Fatalf("user1: %+v", u1)
	}

	u2, err := svc.FindOrCreateUserByExternalID(ctx, chat.PlatformVK, extID, "Иван Петров")
	if err != nil {
		t.Fatal(err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("same identity must map to same user: %d vs %d", u1.ID, u2.ID)
	}

	if got := countIdentitiesFor(t, ctx, pool, chat.PlatformVK, extID); got != 1 {
		t.Fatalf("identity rows for %s: want 1, got %d", extID, got)
	}
	if got := countDistinctUsersFor(t, ctx, pool, chat.PlatformVK, extID); got != 1 {
		t.Fatalf("users for %s: want 1, got %d", extID, got)
	}
}

func TestFindOrCreateUserConcurrentOnce(t *testing.T) {
	ctx, svc, pool := setup(t)
	extID := uniqueExt(t, "user")
	const total = 8

	var wg sync.WaitGroup
	ids := make(chan int64, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := svc.FindOrCreateUserByExternalID(ctx, chat.PlatformVK, extID, "Иван Петров")
			if err != nil {
				t.Errorf("concurrent: %v", err)
				return
			}
			ids <- u.ID
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[int64]bool)
	for id := range ids {
		if len(seen) > 0 && !seen[id] {
			t.Fatalf("concurrent creates must collapse to one user, got id %d", id)
		}
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("want exactly 1 user, got %d distinct ids", len(seen))
	}
	if got := countIdentitiesFor(t, ctx, pool, chat.PlatformVK, extID); got != 1 {
		t.Fatalf("identity rows for %s: want 1, got %d", extID, got)
	}
	if got := countDistinctUsersFor(t, ctx, pool, chat.PlatformVK, extID); got != 1 {
		t.Fatalf("users for %s: want 1, got %d", extID, got)
	}
}

func setup(t *testing.T) (context.Context, *chat.Service, *pgxpool.Pool) {
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
	return ctx, chat.NewService(pool), pool
}

func uniqueExt(t *testing.T, kind string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", kind, time.Now().UnixNano())
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

func insertChannel(t *testing.T, ctx context.Context, pool *pgxpool.Pool, chatID int64, platform, externalChatID string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO chat_channels (chat_id, platform, external_chat_id) VALUES ($1, $2, $3)`,
		chatID, platform, externalChatID,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func countIdentitiesFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, platform, externalID string) int64 {
	t.Helper()
	var n int64
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM user_identities WHERE platform = $1 AND external_user_id = $2`,
		platform, externalID,
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func countDistinctUsersFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, platform, externalID string) int64 {
	t.Helper()
	var n int64
	err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT ui.user_id) FROM user_identities ui
		 WHERE ui.platform = $1 AND ui.external_user_id = $2`,
		platform, externalID,
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
