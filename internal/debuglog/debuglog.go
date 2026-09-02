// Package debuglog is an opt-in diagnostic logger for provider network
// traffic. It exists to answer exactly the question "why did the server
// reject this request" (e.g. a WebDAV 401) without permanently wiring a
// full leveled/rotating application logging framework (a separate,
// not-yet-built feature); this is a narrower, immediately useful stand-in
// for provider HTTP traffic.
//
// It is disabled unless the CLOUDUP_DEBUG environment variable is set, and
// even when enabled it never logs header *values* (which may carry
// credentials/tokens) - only header names, plus the WWW-Authenticate
// response header on error responses, since that header is what actually
// explains a 401/403 (which auth scheme and realm the server expects).
package debuglog

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cloudup/internal/appdir"
)

const envVar = "CLOUDUP_DEBUG"

// Enabled reports whether debug logging was requested for this process.
func Enabled() bool {
	return os.Getenv(envVar) != ""
}

var (
	once   sync.Once
	logger *log.Logger
)

// l lazily opens debug.log in the same "data" directory as config.json and
// history.db (see internal/appdir) and writes to both it and stderr. Both
// destinations are deliberate: in GUI/tray mode there is usually no
// console to read stderr from (see cmd/server's redirectLogToFile), while
// a headless service run has a console but may have no writable data dir.
func l() *log.Logger {
	once.Do(func() {
		var w io.Writer = os.Stderr
		if path, err := logFilePath(); err == nil {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
				if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
					w = io.MultiWriter(os.Stderr, f)
				}
			}
		}
		logger = log.New(w, "[cloudup debug] ", log.LstdFlags|log.Lmicroseconds)
	})
	return logger
}

func logFilePath() (string, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "debug.log"), nil
}

// Transport wraps an http.RoundTripper, logging method/URL/status/duration
// for every request when Enabled() is true (RoundTrip is a no-op passthrough
// otherwise, so providers can wire it unconditionally). RT defaults to
// http.DefaultTransport when nil.
type Transport struct {
	RT http.RoundTripper
}

func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := t.RT
	if rt == nil {
		rt = http.DefaultTransport
	}
	if !Enabled() {
		return rt.RoundTrip(req)
	}

	start := time.Now()
	resp, err := rt.RoundTrip(req)
	elapsed := time.Since(start)

	if err != nil {
		l().Printf("%s %s -> error: %v (%s)", req.Method, req.URL.Redacted(), err, elapsed)
		return resp, err
	}

	l().Printf("%s %s -> %s (%s)", req.Method, req.URL.Redacted(), resp.Status, elapsed)
	if resp.StatusCode >= 400 {
		if wa := resp.Header.Get("Www-Authenticate"); wa != "" {
			l().Printf("  WWW-Authenticate: %s", wa)
		}
	}
	return resp, err
}
