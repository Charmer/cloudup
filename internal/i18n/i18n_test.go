package i18n

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEmbeddedLanguages(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	langs := c.Languages()
	if len(langs) < 2 {
		t.Fatalf("expected at least the two embedded languages, got %d: %+v", len(langs), langs)
	}

	byCode := map[string]string{}
	for _, l := range langs {
		byCode[l.Code] = l.Name
	}
	if byCode["en"] != "English" {
		t.Errorf("en display name = %q, want %q", byCode["en"], "English")
	}
	if byCode["ru"] == "" || byCode["ru"] == "ru" {
		t.Errorf("ru should label itself via %q, got %q", nameKey, byCode["ru"])
	}
}

func TestLanguagesSortedByCode(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	langs := c.Languages()
	for i := 1; i < len(langs); i++ {
		if langs[i-1].Code > langs[i].Code {
			t.Fatalf("languages not sorted by code: %+v", langs)
		}
	}
}

func TestMessagesUnknownLanguageFallsBack(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	en, _ := c.Messages(FallbackLanguage)
	got, installed := c.Messages("kl")
	if installed {
		t.Fatal("Messages reported an uninstalled language as installed")
	}
	if len(got) != len(en) {
		t.Fatalf("fallback map has %d keys, want the %d of %s", len(got), len(en), FallbackLanguage)
	}
	if got["nav.queue"] != en["nav.queue"] {
		t.Errorf("fallback did not return English strings: %q", got["nav.queue"])
	}
}

func TestMessagesReturnsCopy(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	first, _ := c.Messages("en")
	first["nav.queue"] = "mutated"

	second, _ := c.Messages("en")
	if second["nav.queue"] == "mutated" {
		t.Fatal("Messages handed out a reference into the shared catalog")
	}
}

// TestEveryLanguageIsComplete is the guarantee clients rely on: they never
// implement fallback themselves, so a partially translated catalog must be
// completed from English at load time.
func TestEveryLanguageIsComplete(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	en, _ := c.Messages(FallbackLanguage)
	for _, l := range c.Languages() {
		msgs, _ := c.Messages(l.Code)
		for key := range en {
			if key == nameKey {
				continue
			}
			if msgs[key] == "" {
				t.Errorf("language %s is missing key %q", l.Code, key)
			}
		}
	}
}

// TestExternalDirAddsAndOverrides is the whole point of the feature: adding
// a language, and correcting an existing translation, must both work by
// dropping a file - with no rebuild.
func TestExternalDirAddsAndOverrides(t *testing.T) {
	dir := t.TempDir()

	writeCatalog(t, filepath.Join(dir, "kl.json"), map[string]string{
		nameKey:     "Klingon",
		"nav.queue": "tlhIngan queue",
	})
	// Partial file: overrides exactly one key of the embedded English.
	writeCatalog(t, filepath.Join(dir, "en.json"), map[string]string{
		"nav.queue": "Overridden queue",
	})

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	en, installed := c.Messages("en")
	if !installed {
		t.Fatal("en should be installed")
	}
	if en["nav.queue"] != "Overridden queue" {
		t.Errorf("external file did not override embedded key: %q", en["nav.queue"])
	}
	if en["nav.history"] == "" {
		t.Error("a partial external file must not wipe the embedded keys it omits")
	}

	kl, installed := c.Messages("kl")
	if !installed {
		t.Fatal("kl should have been added by the external directory")
	}
	if kl["nav.queue"] != "tlhIngan queue" {
		t.Errorf("kl[nav.queue] = %q", kl["nav.queue"])
	}
	// A brand-new language only translates a few keys; the rest must still
	// resolve, via the fallback completion.
	if kl["nav.history"] != en["nav.history"] {
		t.Errorf("new language was not completed from the fallback: %q", kl["nav.history"])
	}

	var found bool
	for _, l := range c.Languages() {
		if l.Code == "kl" && l.Name == "Klingon" {
			found = true
		}
	}
	if !found {
		t.Errorf("kl missing or unnamed in Languages(): %+v", c.Languages())
	}
}

func TestLoadMissingDirIsNotAnError(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("a missing languages dir must be tolerated, got %v", err)
	}
	if len(c.Languages()) < 2 {
		t.Error("embedded languages should still be present")
	}
}

// TestLoadMalformedFileIsAnError: silently skipping a translation the user
// just installed would look like the feature is broken with no clue why.
func TestLoadMalformedFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"nav.queue": 42}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(dir); err == nil {
		t.Fatal("expected an error for a catalog with a non-string value")
	}
}

func TestLoadIgnoresNonJSONFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("not a catalog"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, l := range c.Languages() {
		if l.Code == "README" {
			t.Error("a non-.json file was picked up as a language")
		}
	}
}

func writeCatalog(t *testing.T, path string, msgs map[string]string) {
	t.Helper()
	data, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
