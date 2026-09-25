package miniapp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sportevents.local/internal/miniapp"
)

// These tests exercise the diagnostic page without VK: it must render the
// launch parameters it was opened with, carry the community id into the payload
// form, and serve VK Bridge from the binary, so that the WebView needs neither
// a CDN nor the VK API.

func TestPageShowsLaunchParametersAndGroup(t *testing.T) {
	h := miniapp.Handler("12345678")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?vk_user_id=42&vk_chat_id=2000000047&vk_platform=mobile_android", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type: %q", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`value="12345678"`,        // VK_GROUP_ID попал в форму app_payload
		"vk_platform",             // страница знает параметры запуска по именам
		"location.search",         // и показывает сырой запрос
		"VKWebAppGetLaunchParams", // параметры можно перечитать у VK
		"VKWebAppGetUserInfo",     // профиль открывшего
		"VKWebAppSendPayload",     // app_payload обратно в сообщество
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestBridgeBundleIsServedFromBinary(t *testing.T) {
	h := miniapp.Handler("")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/vk-bridge.min.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "window.vkBridge") {
		t.Error("bundle does not define window.vkBridge")
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	h := miniapp.Handler("")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: %d, want 404", rec.Code)
	}
}
