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

// miniAppBase is where the mini app is mounted: the pages build their URLs from
// it, and the administrator sets the same address in VK «Размещение». The
// interface lives on <base>/, the diagnostic page on <base>/debug/.
const miniAppBase = "/app"

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

	chats := chat.NewService(pool)
	client := vk.NewClient(os.Getenv("VK_TOKEN"), os.Getenv("VK_GROUP_ID"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(pool))

	// VK is optional: without the token and the confirmation string the bot does
	// not accept callbacks (VK_* vars may be missing on a development box), and
	// then the mini app has no avatars, no names and nowhere to book — it still
	// serves the pages and the games of a connected chat.
	var (
		names    miniapp.NameSource
		photos   miniapp.PhotoSource
		chatSync miniapp.ChatSyncer
	)
	if svc, ok := newVKService(pool, chats, client); ok {
		names, photos, chatSync = client, client, svc
		mux.HandleFunc("POST /vk/callback", svc.HandleCallback)
		log.Printf("vk: /vk/callback is up")
	} else {
		log.Printf("vk: not configured, /vk/callback is disabled")
	}

	app := miniapp.New(miniapp.Config{
		Base:      miniAppBase,
		GroupID:   os.Getenv("VK_GROUP_ID"),
		AppSecret: os.Getenv("VK_APP_SECRET"),
	}, miniapp.Deps{
		Chats:      chats,
		Users:      chats,
		Identities: chats,
		Events:     event.NewService(pool),
		Bookings:   booking.NewService(pool),
		Names:      names,
		Photos:     photos,
		Chat:       chatSync,
	})
	mux.Handle("GET "+miniAppBase+"/", app)
	mux.Handle("POST "+miniAppBase+"/api/", app)
	log.Printf("miniapp: %s/ and %s/debug/ are up", miniAppBase, miniAppBase)

	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// newVKService wires the VK adapter around the chat store. VK is optional:
// without a token and a confirmation string the service runs without the
// callback endpoint.
func newVKService(pool *pgxpool.Pool, chats *chat.Service, client *vk.Client) (*vk.Service, bool) {
	confirmation := os.Getenv("VK_CONFIRMATION_TOKEN")
	token := os.Getenv("VK_TOKEN")
	if confirmation == "" || token == "" {
		return nil, false
	}

	// The VK client is both the messenger and the source of conversation
	// metadata; chat, event, booking and announcement services are the
	// application layer the bot drives.
	return vk.NewService(vk.Config{
		ConfirmationToken: confirmation,
		Secret:            os.Getenv("VK_SECRET"),
		GroupID:           os.Getenv("VK_GROUP_ID"),
		AppID:             os.Getenv("VK_APP_ID"),
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
