package httpapi

import (
	"net/http"
)

// handleLanguagesList reports every installed UI language. Clients use it to
// build a language picker without knowing in advance which languages exist -
// the point of internal/i18n's drop-in external catalogs is that the set is
// not fixed at build time, so it can only be discovered at runtime, and only
// by the server (a browser cannot list a directory).
func (s *Server) handleLanguagesList(w http.ResponseWriter, r *http.Request) {
	if s.Languages == nil {
		writeError(w, http.StatusServiceUnavailable, "translation catalogs are not loaded")
		return
	}
	writeJSON(w, http.StatusOK, s.Languages.Languages())
}

// handleLanguagesGet returns one language's complete key -> string map.
//
// "Complete" is the contract: internal/i18n fills any gaps from English at
// load time, so a client never has to implement fallback itself. An unknown
// code is answered with the fallback language's messages rather than a 404,
// because a UI whose stored language was since removed from the languages
// directory should still render - the response is a usable catalog either
// way.
func (s *Server) handleLanguagesGet(w http.ResponseWriter, r *http.Request) {
	if s.Languages == nil {
		writeError(w, http.StatusServiceUnavailable, "translation catalogs are not loaded")
		return
	}
	msgs, _ := s.Languages.Messages(r.PathValue("code"))
	writeJSON(w, http.StatusOK, msgs)
}
