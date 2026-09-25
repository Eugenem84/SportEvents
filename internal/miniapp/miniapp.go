// Package miniapp serves the diagnostic mini app page mounted at /app/.
//
// The page answers one question before any real interface is written: does VK
// open the mini app from a chat, and what does it put into the launch address
// (vk_user_id, vk_platform, vk_chat_id, sign)? The page shows the raw query,
// what VK Bridge reports, and can send an app_payload back to the community.
// It is a smoke-test tool: it reads nothing from the database.
package miniapp

import (
	"embed"
	"html/template"
	"log"
	"net/http"
)

// assets holds the page and the VK Bridge bundle. The bundle is kept in the
// repository (8 KB) so the page does not depend on a CDN inside the WebView.
//
//go:embed index.html static
var assets embed.FS

// Page is everything index.html needs from the server.
type Page struct {
	// GroupID is the community id (VK_GROUP_ID). VKWebAppSendPayload requires
	// it, so the value pre-fills the form; on a box without VK_* variables it
	// is empty and the person types it by hand.
	GroupID string
}

// Handler serves the page at "/" and its static files below it. Mount it with
// http.StripPrefix("/app", miniapp.Handler(...)).
//
// Every page hit is logged with the raw query string: that log line is the
// point of the route. No launch parameters mean the app was opened outside VK
// (or the URL in the app settings belongs to another platform).
//
// The query is written to the log as is: launch parameters carry no tokens
// (sign is a signature of the parameters, not a secret key), and without the
// full string the smoke test cannot tell what VK actually sent.
func Handler(groupID string) http.Handler {
	page := template.Must(template.ParseFS(assets, "index.html"))
	files := http.FileServerFS(assets)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			files.ServeHTTP(w, r)
			return
		}

		log.Printf("miniapp: page hit path=%s query=%q ua=%q", r.URL.Path, r.URL.RawQuery, r.UserAgent())

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := page.Execute(w, Page{GroupID: groupID}); err != nil {
			log.Printf("miniapp: render: %v", err)
		}
	})
}
