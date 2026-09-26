package chat_test

import (
	"context"
	"errors"
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

// --- Connect / IsChatAdmin (Phase 5) ---

func TestConnectCreatesChatChannelAndAdmin(t *testing.T) {
	ctx, svc, pool := setup(t)
	userID := insertUser(t, ctx, pool, "Иван Петров")
	extID := uniqueExt(t, "conv")

	c, created, err := svc.Connect(ctx, chat.ConnectInput{
		Platform:       chat.PlatformVK,
		ExternalChatID: extID,
		ChatTitle:      "Волейбол Иваново",
		InitiatorID:    userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first connect must create a chat")
	}
	if c.ID == 0 || c.Title != "Волейбол Иваново" {
		t.Fatalf("chat: %+v", c)
	}

	found, err := svc.FindByChannel(ctx, chat.PlatformVK, extID)
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || found.ID != c.ID {
		t.Fatalf("channel must point at the new chat, got %+v", found)
	}

	ok, err := svc.IsChatAdmin(ctx, c.ID, userID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the initiator must become a chat admin")
	}
	if got := countAdmins(t, ctx, pool, c.ID); got != 1 {
		t.Fatalf("admins: want 1, got %d", got)
	}
}

// Reconnecting an already connected conversation must return the existing
// Chat and neither create a second one nor change its title or admins.
func TestConnectIdempotent(t *testing.T) {
	ctx, svc, pool := setup(t)
	userA := insertUser(t, ctx, pool, "Иван")
	userB := insertUser(t, ctx, pool, "Пётр")
	extID := uniqueExt(t, "conv")

	first, created, err := svc.Connect(ctx, chat.ConnectInput{
		Platform:       chat.PlatformVK,
		ExternalChatID: extID,
		ChatTitle:      "Волейбол Иваново",
		InitiatorID:    userA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first connect must create a chat")
	}

	second, created, err := svc.Connect(ctx, chat.ConnectInput{
		Platform:       chat.PlatformVK,
		ExternalChatID: extID,
		ChatTitle:      "Другое название",
		InitiatorID:    userB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second connect must not create a chat")
	}
	if second.ID != first.ID || second.Title != first.Title {
		t.Fatalf("second connect must return the existing chat: %+v vs %+v", second, first)
	}
	if got := countChannelsFor(t, ctx, pool, chat.PlatformVK, extID); got != 1 {
		t.Fatalf("channels: want 1, got %d", got)
	}
	if got := countAdmins(t, ctx, pool, first.ID); got != 1 {
		t.Fatalf("admins: want only the first initiator, got %d", got)
	}
}

// Concurrent connects (a retried callback, another bot instance) must
// collapse to one Chat: the UNIQUE (platform, external_chat_id) index
// serializes them and the losers roll back their own Chat row.
func TestConnectConcurrentOnce(t *testing.T) {
	ctx, svc, pool := setup(t)
	userID := insertUser(t, ctx, pool, "Иван")
	extID := uniqueExt(t, "conv")
	title := "Волейбол " + extID
	const total = 8

	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := make([]int64, 0, total)
	createdCount := 0
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, created, err := svc.Connect(ctx, chat.ConnectInput{
				Platform:       chat.PlatformVK,
				ExternalChatID: extID,
				ChatTitle:      title,
				InitiatorID:    userID,
			})
			if err != nil {
				t.Errorf("concurrent connect: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids = append(ids, c.ID)
			if created {
				createdCount++
			}
		}()
	}
	wg.Wait()

	if len(ids) != total {
		t.Fatalf("connects: want %d results, got %d", total, len(ids))
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent connects must return one chat, got id %d and %d", ids[0], id)
		}
	}
	if createdCount != 1 {
		t.Fatalf("exactly one connect must report created=true, got %d", createdCount)
	}
	if got := countChatsWithTitle(t, ctx, pool, title); got != 1 {
		t.Fatalf("orphan chats from losing transactions: want 1 chat titled %q, got %d", title, got)
	}
	if got := countChannelsFor(t, ctx, pool, chat.PlatformVK, extID); got != 1 {
		t.Fatalf("channels: want 1, got %d", got)
	}
	if got := countAdmins(t, ctx, pool, ids[0]); got != 1 {
		t.Fatalf("admins: want 1, got %d", got)
	}
}

func TestConnectInvalidInput(t *testing.T) {
	ctx, svc, pool := setup(t)
	userID := insertUser(t, ctx, pool, "Иван")
	extID := uniqueExt(t, "conv")

	cases := map[string]chat.ConnectInput{
		"no platform":    {ExternalChatID: extID, ChatTitle: "Волейбол", InitiatorID: userID},
		"no channel":     {Platform: chat.PlatformVK, ChatTitle: "Волейбол", InitiatorID: userID},
		"no title":       {Platform: chat.PlatformVK, ExternalChatID: extID, InitiatorID: userID},
		"no initiator":   {Platform: chat.PlatformVK, ExternalChatID: extID, ChatTitle: "Волейбол"},
		"blank title":    {Platform: chat.PlatformVK, ExternalChatID: extID, ChatTitle: "  ", InitiatorID: userID},
		"blank channel":  {Platform: chat.PlatformVK, ExternalChatID: " ", ChatTitle: "Волейбол", InitiatorID: userID},
		"blank platform": {Platform: " ", ExternalChatID: extID, ChatTitle: "Волейбол", InitiatorID: userID},
	}

	for name, in := range cases {
		if _, _, err := svc.Connect(ctx, in); !errors.Is(err, chat.ErrInvalidConnect) {
			t.Fatalf("%s: want ErrInvalidConnect, got %v", name, err)
		}
	}
}

func TestIsChatAdminFalse(t *testing.T) {
	ctx, svc, pool := setup(t)
	userA := insertUser(t, ctx, pool, "Иван")
	userB := insertUser(t, ctx, pool, "Пётр")
	extID := uniqueExt(t, "conv")

	c, _, err := svc.Connect(ctx, chat.ConnectInput{
		Platform:       chat.PlatformVK,
		ExternalChatID: extID,
		ChatTitle:      "Волейбол",
		InitiatorID:    userA,
	})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := svc.IsChatAdmin(ctx, c.ID, userB)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a non-initiator must not be a chat admin")
	}
}

// SingleChatPeer узнаёт беседу, когда приложение открыто в контексте сообщества:
// одна подключённая беседа даёт однозначный ответ, две и больше — нет, и тогда
// приложение не угадывает, а честно просит открыть его из чата.
func TestSingleChatPeer(t *testing.T) {
	ctx, svc, pool := setup(t)

	// Свой platform на каждый прогон: таблица общая, а метод спрашивает по
	// платформе, поэтому чужие подключения других тестов ответ не портят.
	platform := uniqueExt(t, "platform")
	chatA := insertChat(t, ctx, pool, "Волейбол")
	insertChannel(t, ctx, pool, chatA, platform, "2000000101")

	peer, ok, err := svc.SingleChatPeer(ctx, platform)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || peer != "2000000101" {
		t.Fatalf("single chat: peer %q ok %v, want 2000000101/true", peer, ok)
	}

	// Вторая беседа делает ответ неоднозначным.
	chatB := insertChat(t, ctx, pool, "Волейбол-2")
	insertChannel(t, ctx, pool, chatB, platform, "2000000102")

	if peer, ok, err = svc.SingleChatPeer(ctx, platform); err != nil {
		t.Fatal(err)
	} else if ok || peer != "" {
		t.Fatalf("two chats must not resolve: peer %q ok %v", peer, ok)
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

func insertUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO users (display_name) VALUES ($1) RETURNING id`, name).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func countChannelsFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, platform, externalID string) int64 {
	t.Helper()
	var n int64
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_channels WHERE platform = $1 AND external_chat_id = $2`,
		platform, externalID,
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func countChatsWithTitle(t *testing.T, ctx context.Context, pool *pgxpool.Pool, title string) int64 {
	t.Helper()
	var n int64
	err := pool.QueryRow(ctx, `SELECT count(*) FROM chats WHERE title = $1`, title).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Аватарки в мини-приложении строятся от VK-идентификаторов: у записи есть
// только внутренний пользователь, и мостик между ними — user_identities.
func TestIdentitiesByUsers(t *testing.T) {
	ctx, svc, pool := setup(t)

	external := uniqueExt(t, "vk")
	linked, err := svc.FindOrCreateUserByExternalID(ctx, chat.PlatformVK, external, "Евгений")
	if err != nil {
		t.Fatal(err)
	}
	guest := insertUser(t, ctx, pool, "Гость без профиля")

	got, err := svc.IdentitiesByUsers(ctx, chat.PlatformVK, []int64{linked.ID, guest})
	if err != nil {
		t.Fatal(err)
	}
	if got[linked.ID] != external {
		t.Fatalf("identity: %+v", got)
	}
	if _, ok := got[guest]; ok {
		t.Fatalf("a user without an identity must be absent: %+v", got)
	}
}

func TestIdentitiesByUsersEmptyInput(t *testing.T) {
	ctx, svc, _ := setup(t)

	got, err := svc.IdentitiesByUsers(ctx, chat.PlatformVK, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want an empty map, got %+v", got)
	}
}

func countAdmins(t *testing.T, ctx context.Context, pool *pgxpool.Pool, chatID int64) int64 {
	t.Helper()
	var n int64
	err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_admins WHERE chat_id = $1`, chatID).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
