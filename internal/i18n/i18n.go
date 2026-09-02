// Package i18n serves UI translation catalogs over the REST API, so a new
// language can be added by dropping one JSON file next to the executable -
// without rebuilding either the Go binary or the frontend.
//
// Why the backend owns this at all, given that localization is otherwise
// entirely the frontend's concern: the frontend is deliberately replaceable
// (served from disk, see internal/httpapi's staticHandler, and any
// third-party client can be written against openapi.yaml). Making the
// catalogs part of the documented API rather than static files inside one
// particular frontend build means every client gets the same languages, the
// server can enumerate what's installed (a browser cannot list a directory),
// and the built-in languages keep working even when no external directory
// exists.
//
// A catalog is a flat JSON object of key -> string. Two languages (en, ru)
// are embedded in the binary so they are always available; files found in
// the external directory (see DefaultDir) are layered on top - a file whose
// name matches an embedded language overrides individual keys of it, a file
// with a new name adds a whole new language. Keys absent from a language
// fall back to English at load time, so Messages always returns a complete
// map and clients need no fallback logic of their own.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed languages/*.json
var embedded embed.FS

// FallbackLanguage is the language every other catalog is completed from,
// and the one served when a client asks for a language that isn't
// installed. It must always exist among the embedded catalogs.
const FallbackLanguage = "en"

// nameKey is the reserved catalog key holding a language's own display name
// ("English", "Русский"). It lets a new language label itself, so adding
// one really is just dropping a file - no Go or frontend change needed to
// make it appear correctly in a language picker.
const nameKey = "__name__"

// Language describes one installed language for a picker UI.
type Language struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Catalog holds every installed language's messages. Build one with Load.
type Catalog struct {
	// messages maps language code -> (key -> string). Every non-fallback
	// language has already been completed from the fallback at Load time.
	messages map[string]map[string]string
}

// Load builds a Catalog from the embedded languages, then overlays any
// *.json found in dir (non-recursively). An empty dir, or a dir that does
// not exist, is not an error - the embedded languages alone are a valid
// catalog, which is what makes the external directory purely optional.
//
// A malformed external file IS reported as an error rather than skipped:
// silently ignoring a translation the user just installed would look like
// the feature is broken with no clue as to why.
func Load(dir string) (*Catalog, error) {
	c := &Catalog{messages: map[string]map[string]string{}}

	entries, err := embedded.ReadDir("languages")
	if err != nil {
		return nil, fmt.Errorf("i18n: reading embedded languages: %w", err)
	}
	for _, e := range entries {
		code := languageCode(e.Name())
		if code == "" {
			continue
		}
		data, err := embedded.ReadFile(filepath.ToSlash(filepath.Join("languages", e.Name())))
		if err != nil {
			return nil, fmt.Errorf("i18n: reading embedded %s: %w", e.Name(), err)
		}
		msgs, err := parseCatalog(data)
		if err != nil {
			return nil, fmt.Errorf("i18n: embedded %s: %w", e.Name(), err)
		}
		c.messages[code] = msgs
	}

	if _, ok := c.messages[FallbackLanguage]; !ok {
		return nil, fmt.Errorf("i18n: embedded catalogs are missing the %q fallback language", FallbackLanguage)
	}

	if dir != "" {
		if err := c.overlayDir(dir); err != nil {
			return nil, err
		}
	}

	c.completeFromFallback()
	return c, nil
}

// overlayDir merges every *.json in dir into the catalog.
func (c *Catalog) overlayDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("i18n: reading %s: %w", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		code := languageCode(e.Name())
		if code == "" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("i18n: reading %s: %w", path, err)
		}
		msgs, err := parseCatalog(data)
		if err != nil {
			return fmt.Errorf("i18n: %s: %w", path, err)
		}
		if existing, ok := c.messages[code]; ok {
			// Per-key override of an embedded language: a partial external
			// file only replaces the keys it actually contains.
			for k, v := range msgs {
				existing[k] = v
			}
			continue
		}
		c.messages[code] = msgs
	}
	return nil
}

// completeFromFallback fills gaps in every language from the fallback, so
// Messages never returns a partially translated map and clients never have
// to implement fallback themselves. The language's own nameKey is left
// alone - inheriting "English" as a Russian catalog's display name would be
// worse than leaving it empty.
func (c *Catalog) completeFromFallback() {
	base := c.messages[FallbackLanguage]
	for code, msgs := range c.messages {
		if code == FallbackLanguage {
			continue
		}
		for k, v := range base {
			if k == nameKey {
				continue
			}
			if _, ok := msgs[k]; !ok {
				msgs[k] = v
			}
		}
	}
}

// Languages lists every installed language, sorted by code for a stable
// picker order. A language with no nameKey falls back to labelling itself
// by its own code, which is still usable.
func (c *Catalog) Languages() []Language {
	out := make([]Language, 0, len(c.messages))
	for code, msgs := range c.messages {
		name := msgs[nameKey]
		if name == "" {
			name = code
		}
		out = append(out, Language{Code: code, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// Messages returns the complete message map for code. Unknown codes resolve
// to the fallback language rather than an error: a client asking for a
// language that was removed from the languages directory should still get a
// usable UI. The bool reports whether code itself was installed, so a
// caller that cares (an API handler wanting to 404) can tell the difference.
func (c *Catalog) Messages(code string) (map[string]string, bool) {
	msgs, ok := c.messages[code]
	if !ok {
		msgs = c.messages[FallbackLanguage]
	}
	// Copy: the catalog is shared by concurrent HTTP handlers and must not
	// be mutable through a returned reference.
	out := make(map[string]string, len(msgs))
	for k, v := range msgs {
		out[k] = v
	}
	return out, ok
}

// DefaultDir is the external languages directory: "languages" next to the
// running executable. Chosen over the user-config dir so that installing a
// translation is "drop the file next to the program" - the obvious gesture
// for a desktop app. Server deployments where that path is read-only can
// point elsewhere via cmd/server's -languages flag.
func DefaultDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("i18n: locating executable: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "languages"), nil
}

// languageCode maps a catalog filename to its language code, returning ""
// for anything that isn't a .json file.
func languageCode(filename string) string {
	if !strings.EqualFold(filepath.Ext(filename), ".json") {
		return ""
	}
	return strings.TrimSuffix(filename, filepath.Ext(filename))
}

func parseCatalog(data []byte) (map[string]string, error) {
	var msgs map[string]string
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("invalid catalog JSON (expected a flat object of string values): %w", err)
	}
	if msgs == nil {
		msgs = map[string]string{}
	}
	return msgs, nil
}
