package miniapp_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
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

// TestPageCarriesServerTestMarker covers the test output at the top of the
// page: the Go template stamps the moment it rendered the page, and the script
// fills the JavaScript row. Together they tell a live page from a cached one
// and a working WebView from one that does not run scripts.
func TestPageCarriesServerTestMarker(t *testing.T) {
	h := miniapp.Handler("")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"Тестовый вывод",  // заголовок блока
		`id="probeJs"`,    // строка, которую заполняет скрипт
		"не выполнился",   // её исходное значение: без JS видно, что скрипт не пошёл
		"Go-шаблон",       // подпись серверной метки
		"локальное время", // подпись, которую подставляет скрипт
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}

	// Server render time is RFC3339 in UTC, e.g. 2026-09-25T21:39:18Z.
	if !regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`).MatchString(body) {
		t.Error("page does not contain the RFC3339 render time (Page.ServedAt)")
	}
	if strings.Contains(body, "{{.ServedAt}}") {
		t.Error("template placeholder left unresolved: page served without the data")
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
