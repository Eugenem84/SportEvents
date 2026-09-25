package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"sportevents.local/internal/announce"
	"sportevents.local/internal/booking"
	"sportevents.local/internal/chat"
	"sportevents.local/internal/event"
	"sportevents.local/internal/miniapp"
	"sportevents.local/internal/postgres"
	"sportevents.local/internal/schedule"
	"sportevents.local/internal/vk"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	pool, err := postgres.NewPool(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	if err := postgres.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}

	addr := ":8080"
	if v := os.Getenv("HTTP_ADDR"); v != "" {
		addr = v
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(pool))

	// The diagnostic mini app page: VK opens https://<host>/app/ in a WebView
	// and appends the launch parameters. The page reads nothing from the
	// database, so it is mounted whether or not VK_* variables are configured.
	mux.Handle("GET /app/", http.StripPrefix("/app", miniapp.Handler(os.Getenv("VK_GROUP_ID"))))

	if svc, ok := newVKService(pool); ok {
		mux.HandleFunc("POST /vk/callback", svc.HandleCallback)
		log.Printf("vk: /vk/callback is up")
	} else {
		log.Printf("vk: not configured, /vk/callback is disabled")
	}

	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// newVKService wires the VK adapter around the chat store. VK is optional:
// without a token and confirmation string the service runs without the
// callback endpoint (VK_* vars may be missing on a development box).
func newVKService(pool *pgxpool.Pool) (*vk.Service, bool) {
	confirmation := os.Getenv("VK_CONFIRMATION_TOKEN")
	token := os.Getenv("VK_TOKEN")
	if confirmation == "" || token == "" {
		return nil, false
	}

	client := vk.NewClient(token, os.Getenv("VK_GROUP_ID"))
	chats := chat.NewService(pool)
	// The VK client is both the messenger and the source of conversation
	// metadata; chat, event, booking and announcement services are the
	// application layer the bot drives.
	return vk.NewService(vk.Config{
		ConfirmationToken: confirmation,
		Secret:            os.Getenv("VK_SECRET"),
		GroupID:           os.Getenv("VK_GROUP_ID"),
	}, vk.Deps{
		Messenger: client,
		Chats:     chats,
		Users:     chats,
		Convs:     client,
		Events:    event.NewService(pool),
		Bookings:  booking.NewService(pool),
		Announces: announce.NewService(pool),
		Schedule:  schedule.NewService(pool),
	}), true
}

func healthHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
