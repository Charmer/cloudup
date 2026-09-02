package httpapi

import (
	"net/http"
	"testing"

	"cloudup/internal/i18n"
)

// testCatalog loads the embedded translation catalogs (an empty overlay
// directory adds nothing), so language tests do not depend on anything
// installed next to the test binary.
func testCatalog(t *testing.T) *i18n.Catalog {
	t.Helper()
	c, err := i18n.Load(t.TempDir())
	if err != nil {
		t.Fatalf("i18n.Load() error = %v", err)
	}
	return c
}

// TestLanguagesListReportsInstalledLanguages: the endpoint exists so a
// client can build a language picker without hardcoding the set - so the
// response must carry a code *and* a display name for each.
func TestLanguagesListReportsInstalledLanguages(t *testing.T) {
	env := newTestEnv(t, func(s *Server) { s.Languages = testCatalog(t) })

	langs := decodeBody[[]i18n.Language](t, env.do(http.MethodGet, "/api/v1/languages", nil), http.StatusOK)
	if len(langs) < 2 {
		t.Fatalf("languages = %+v, want at least the two built-in ones", langs)
	}
	byCode := map[string]string{}
	for _, l := range langs {
		if l.Code == "" || l.Name == "" {
			t.Errorf("language entry with an empty field: %+v", l)
		}
		byCode[l.Code] = l.Name
	}
	for _, code := range []string{"en", "ru"} {
		if _, ok := byCode[code]; !ok {
			t.Errorf("built-in language %q missing from the list: %+v", code, langs)
		}
	}
}

// TestLanguagesGetReturnsCompleteCatalog - "complete" is the documented
// contract: i18n fills gaps from English at load time so a client never has
// to implement fallback itself. Every installed language must therefore
// have the same key set as English.
func TestLanguagesGetReturnsCompleteCatalog(t *testing.T) {
	env := newTestEnv(t, func(s *Server) { s.Languages = testCatalog(t) })

	en := decodeBody[map[string]string](t, env.do(http.MethodGet, "/api/v1/languages/en", nil), http.StatusOK)
	if len(en) == 0 {
		t.Fatal("english catalog is empty")
	}
	ru := decodeBody[map[string]string](t, env.do(http.MethodGet, "/api/v1/languages/ru", nil), http.StatusOK)
	for key := range en {
		if _, ok := ru[key]; !ok {
			t.Errorf("russian catalog is missing key %q - it was not completed from the fallback", key)
		}
	}
}

// TestLanguagesGetUnknownCodeFallsBack: a UI whose stored language was
// removed from the languages directory must still render, so an unknown
// code returns the fallback catalog rather than a 404.
func TestLanguagesGetUnknownCodeFallsBack(t *testing.T) {
	env := newTestEnv(t, func(s *Server) { s.Languages = testCatalog(t) })

	en := decodeBody[map[string]string](t, env.do(http.MethodGet, "/api/v1/languages/en", nil), http.StatusOK)
	got := decodeBody[map[string]string](t, env.do(http.MethodGet, "/api/v1/languages/kl", nil), http.StatusOK)
	if len(got) != len(en) {
		t.Fatalf("unknown-code catalog has %d keys, want the fallback's %d", len(got), len(en))
	}
	for key, want := range en {
		if got[key] != want {
			t.Errorf("unknown-code catalog key %q = %q, want the fallback's %q", key, got[key], want)
			break
		}
	}
}

// TestLanguagesReport503WhenCatalogsAbsent: the API stays fully usable
// without translation catalogs (a client can ship its own strings), so
// these two endpoints report an explicit 503 rather than panicking on a nil
// catalog or pretending no languages exist.
func TestLanguagesReport503WhenCatalogsAbsent(t *testing.T) {
	env := newTestEnv(t)
	if env.Server.Languages != nil {
		t.Fatal("Languages should be nil by default in this test env")
	}

	for _, path := range []string{"/api/v1/languages", "/api/v1/languages/en"} {
		msg := errorMessage(t, env.do(http.MethodGet, path, nil), http.StatusServiceUnavailable)
		if msg == "" {
			t.Errorf("GET %s: empty error message", path)
		}
	}
}
