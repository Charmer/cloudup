package webdav

import (
	"net/http"

	"github.com/studio-b12/gowebdav"
)

// preemptiveBasicAuth implements gowebdav.Authenticator by sending Basic
// auth on every request up front, rather than gowebdav's default
// (gowebdav.NewAutoAuth, used by gowebdav.NewClient) of probing with an
// unauthenticated request first and retrying once challenged.
//
// That default negotiate-then-retry flow is broken for anything but small
// uploads: gowebdav's retry path replays the request body from a
// bytes.Buffer it fills by tee-ing the body as the *first* (unauthenticated)
// request is written - but net/http's transport writes a request body and
// reads its response concurrently, so a server that responds 401 quickly
// (as most do, without reading the body at all) races the retry's read of
// that buffer against the still-in-flight write of the original body.
// Observed against Yandex.Disk as a PUT that logs a single 401 and then
// never completes - the upload silently never lands. Sending credentials
// preemptively means the server accepts the first request outright, so the
// buffer-and-retry path (and its race) is never exercised.
//
// Verify always reports "no redo needed": if the server still rejects a
// preemptive Basic request, retrying with the exact same header would not
// help, so surfacing the error as-is (via gowebdav's normal non-2xx
// handling) is more useful than a silent extra round trip.
type preemptiveBasicAuth struct {
	user, pw string
}

func newPreemptiveBasicAuth(user, pw string) gowebdav.Authorizer {
	return gowebdav.NewPreemptiveAuth(preemptiveBasicAuth{user: user, pw: pw})
}

func (a preemptiveBasicAuth) Authorize(c *http.Client, rq *http.Request, path string) error {
	rq.SetBasicAuth(a.user, a.pw)
	return nil
}

func (a preemptiveBasicAuth) Verify(c *http.Client, rs *http.Response, path string) (redo bool, err error) {
	return false, nil
}

func (a preemptiveBasicAuth) Close() error { return nil }

func (a preemptiveBasicAuth) Clone() gowebdav.Authenticator { return a }

func (a preemptiveBasicAuth) String() string { return "PreemptiveBasicAuth login: " + a.user }
