package miniapp

import (
	"net/url"
	"testing"
)

// The official example from the VK documentation («Подпись параметров запуска»):
// the launch parameters below and the protected key must produce exactly the
// signature VK published. If this test breaks, the check will reject every real
// launch — that is the whole point of pinning it.
func TestSignMatchesOfficialExample(t *testing.T) {
	q := url.Values{
		"vk_user_id":                   {"494075"},
		"vk_app_id":                    {"6736218"},
		"vk_is_app_user":               {"1"},
		"vk_are_notifications_enabled": {"1"},
		"vk_language":                  {"ru"},
		"vk_access_token_settings":     {""},
		"vk_platform":                  {"android"},
		"sign":                         {"htQFduJpLxz7ribXRZpDFUH-XEUhC9rBPTJkjUFEkRA"},
	}

	if !signMatches(q, "wvl68m4dR1UpLrVRli") {
		t.Fatal("the signature from the documentation must verify")
	}
	if signMatches(q, "another-secret") {
		t.Fatal("a wrong protected key must not verify")
	}
}

// VK signs only the vk_ parameters: our own ones — the conversation in the launch
// hash, for instance — must not change the string, otherwise the check would fail
// for every launch made through the «Открыть приложение» button.
func TestSignIgnoresForeignParameters(t *testing.T) {
	base := url.Values{
		"vk_user_id":                   {"494075"},
		"vk_app_id":                    {"6736218"},
		"vk_is_app_user":               {"1"},
		"vk_are_notifications_enabled": {"1"},
		"vk_language":                  {"ru"},
		"vk_access_token_settings":     {""},
		"vk_platform":                  {"android"},
		"sign":                         {"htQFduJpLxz7ribXRZpDFUH-XEUhC9rBPTJkjUFEkRA"},
	}
	// The same launch, but with the parameters the mini app adds itself.
	base.Set("hash", "peer=2000000047")
	base.Set("event_id", "7")

	if !signMatches(base, "wvl68m4dR1UpLrVRli") {
		t.Fatal("parameters outside vk_ must not affect the signature")
	}

	// A tampered launch parameter, on the other hand, must be caught.
	base.Set("vk_user_id", "1")
	if signMatches(base, "wvl68m4dR1UpLrVRli") {
		t.Fatal("a tampered vk_ parameter must not verify")
	}
}

func TestSignWithoutVKParameters(t *testing.T) {
	q := url.Values{"hash": {"peer=2000000047"}, "sign": {"whatever"}}
	if signMatches(q, "secret") {
		t.Fatal("without vk_ parameters there is nothing to verify")
	}
}

// encodeURIComponent follows the JavaScript reference implementation: commas and
// spaces are percent-encoded, the unreserved set is not, and bytes of UTF-8 are
// encoded one by one.
func TestEncodeURIComponent(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"friends,stories": "friends%2Cstories",
		"a b":             "a%20b",
		"Волейбол":        "%D0%92%D0%BE%D0%BB%D0%B5%D0%B9%D0%B1%D0%BE%D0%BB",
		"A-z_0.9-!~*'()":  "A-z_0.9-!~*'()",
		"1+2=3&4":         "1%2B2%3D3%264",
		"mobile_android":  "mobile_android",
	}
	for in, want := range cases {
		if got := encodeURIComponent(in); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", in, got, want)
		}
	}
}
