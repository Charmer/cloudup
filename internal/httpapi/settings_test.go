package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudup/internal/provider"
	"cloudup/internal/settings"
)

// TestSettingsGetReturnsDefaultsOnFreshStore - a client's first GET must be
// a usable settings object, not an empty one it would have to guess
// defaults for.
func TestSettingsGetReturnsDefaultsOnFreshStore(t *testing.T) {
	env := newTestEnv(t)

	got := decodeBody[settings.Settings](t, env.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)
	if want := settings.Default(); !reflect.DeepEqual(got, want) {
		t.Errorf("settings = %+v, want the package defaults %+v", got, want)
	}
}

// TestSettingsResponseHasNoThemeField pins the response shape against a
// field that was deliberately removed: theme is the frontend's own
// business, and openapi.yaml no longer documents it. Decoding into the
// struct would not notice a stray key, so the raw JSON is inspected.
func TestSettingsResponseHasNoThemeField(t *testing.T) {
	env := newTestEnv(t)

	raw := decodeBody[map[string]any](t, env.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)
	if _, present := raw["theme"]; present {
		t.Errorf("settings response carries a theme field: %v", raw)
	}
	wantKeys := []string{"maxConcurrentUploadsPerProvider", "verifyChecksumAfterUpload", "language", "multiThreadStreams", "multiThreadThresholdBytes", "maxUploadBytesPerSecond", "idleConnectionTimeoutMinutes"}
	if len(raw) != len(wantKeys) {
		t.Errorf("settings response keys = %v, want exactly %v", raw, wantKeys)
	}
	for _, key := range wantKeys {
		if _, present := raw[key]; !present {
			t.Errorf("settings response missing %q: %v", key, raw)
		}
	}
}

// TestSettingsPutPersistsAndEchoesNormalizedValues: the PUT response is the
// *stored* settings, so a value the store normalizes (concurrency < 1) is
// reported back as normalized rather than as submitted.
func TestSettingsPutPersistsAndEchoesNormalizedValues(t *testing.T) {
	env := newTestEnv(t)

	saved := decodeBody[settings.Settings](t, env.doJSON(http.MethodPut, "/api/v1/settings", settings.Settings{
		MaxConcurrentUploadsPerProvider: 4,
		VerifyChecksumAfterUpload:       map[string]bool{fakeType: true},
		Language:                        "ru",
	}), http.StatusOK)
	if saved.MaxConcurrentUploadsPerProvider != 4 || !saved.VerifyChecksumAfterUpload[fakeType] || saved.Language != "ru" {
		t.Fatalf("PUT response = %+v, want the submitted values", saved)
	}

	got := decodeBody[settings.Settings](t, env.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)
	if !reflect.DeepEqual(got, saved) {
		t.Errorf("GET after PUT = %+v, want %+v", got, saved)
	}

	normalized := decodeBody[settings.Settings](t, env.doJSON(http.MethodPut, "/api/v1/settings", settings.Settings{
		MaxConcurrentUploadsPerProvider: 0,
		Language:                        "",
	}), http.StatusOK)
	if normalized.MaxConcurrentUploadsPerProvider != 1 {
		t.Errorf("concurrency 0 stored as %d, want it clamped to 1", normalized.MaxConcurrentUploadsPerProvider)
	}
	if normalized.Language != settings.DefaultLanguage {
		t.Errorf("empty language stored as %q, want %q", normalized.Language, settings.DefaultLanguage)
	}
	if normalized.IdleConnectionTimeoutMinutes != settings.DefaultIdleConnectionTimeoutMinutes {
		t.Errorf("idle connection timeout 0 stored as %d, want default %d", normalized.IdleConnectionTimeoutMinutes, settings.DefaultIdleConnectionTimeoutMinutes)
	}
}

// TestSettingsPutRejectsInvalidBody - a malformed or unknown-field body must
// not partially apply.
func TestSettingsPutRejectsInvalidBody(t *testing.T) {
	env := newTestEnv(t)

	errorMessage(t, env.do(http.MethodPut, "/api/v1/settings", strings.NewReader("not json")), http.StatusBadRequest)
	errorMessage(t, env.do(http.MethodPut, "/api/v1/settings",
		strings.NewReader(`{"theme":"dark"}`)), http.StatusBadRequest)

	got := decodeBody[settings.Settings](t, env.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)
	if !reflect.DeepEqual(got, settings.Default()) {
		t.Errorf("settings changed after a rejected PUT: %+v", got)
	}
}

// TestSettingsPutAppliesConcurrencyToTheLiveQueue asserts the behavior the
// handler's comment promises: no restart needed. It is checked
// behaviorally - two uploads on the *same* connection must be able to run
// at once after raising the limit, which is impossible at the default of 1.
func TestSettingsPutAppliesConcurrencyToTheLiveQueue(t *testing.T) {
	env := newTestEnv(t)

	if rec := env.doJSON(http.MethodPut, "/api/v1/settings", settings.Settings{
		MaxConcurrentUploadsPerProvider: 2,
		Language:                        "en",
	}); rec.Code != http.StatusOK {
		t.Fatalf("PUT settings status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	conn := env.createConnection(fakeType, "Concurrent", map[string]string{"url": "u"}, nil)

	var mu sync.Mutex
	inFlight := 0
	release := make(chan struct{})
	behaviorFor(t, conn.ID).upload = func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		mu.Lock()
		inFlight++
		mu.Unlock()
		<-release
		return provider.UploadResult{RemotePath: task.RemotePath}, nil
	}

	for i := 0; i < 2; i++ {
		body, contentType := multipartUpload(t, "a.txt", "/a.txt", []byte("payload"))
		req := env.newUploadRequest(conn.ID, body, contentType)
		if rec := env.serve(req); rec.Code != http.StatusAccepted {
			t.Fatalf("upload %d status = %d, want 202 (body: %s)", i, rec.Code, rec.Body.String())
		}
	}

	waitFor(t, 3*time.Second, "two uploads to run at once", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return inFlight == 2
	})
	close(release)
}

// TestSettingsPutAppliesVerifyAfterUploadToTheLiveQueue - the second
// setting queue.Manager can apply live. Verification runs only when the
// setting is on, so a call to the provider's VerifyChecksum after a
// successful upload is the observable proof it was pushed through.
func TestSettingsPutAppliesVerifyAfterUploadToTheLiveQueue(t *testing.T) {
	env := newTestEnv(t)

	if rec := env.doJSON(http.MethodPut, "/api/v1/settings", settings.Settings{
		MaxConcurrentUploadsPerProvider: 1,
		VerifyChecksumAfterUpload:       map[string]bool{fakeType: true},
		Language:                        "en",
	}); rec.Code != http.StatusOK {
		t.Fatalf("PUT settings status = %d, want 200", rec.Code)
	}

	conn := env.createConnection(fakeType, "Verified", map[string]string{"url": "u"}, nil)

	verified := make(chan string, 1)
	behaviorFor(t, conn.ID).verify = func(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
		select {
		case verified <- remotePath:
		default:
		}
		return true, nil
	}

	body, contentType := multipartUpload(t, "v.txt", "/v.txt", []byte("payload"))
	if rec := env.serve(env.newUploadRequest(conn.ID, body, contentType)); rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, want 202", rec.Code)
	}

	select {
	case got := <-verified:
		if got != "/v.txt" {
			t.Errorf("verified remote path = %q, want /v.txt", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("VerifyChecksum was never called - verifyChecksumAfterUpload did not reach the queue")
	}
}

// TestSettingsPutAppliesMultiThreadStreamsToTheLiveQueue - the third setting
// queue.Manager can apply live. MultiThreadThresholdBytes is set far below
// any real upload's size (1 byte) so even the tiny multipart payload these
// tests use crosses it, making UploadMultipartParallel's call the
// observable proof the setting reached the queue.
func TestSettingsPutAppliesMultiThreadStreamsToTheLiveQueue(t *testing.T) {
	env := newTestEnv(t)

	if rec := env.doJSON(http.MethodPut, "/api/v1/settings", settings.Settings{
		MaxConcurrentUploadsPerProvider: 1,
		Language:                        "en",
		MultiThreadStreams:              2,
		MultiThreadThresholdBytes:       1,
	}); rec.Code != http.StatusOK {
		t.Fatalf("PUT settings status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	conn := env.createConnection(fakeType, "MultiThreaded", map[string]string{"url": "u"}, nil)

	called := make(chan int, 1)
	behaviorFor(t, conn.ID).parallelMultipart = func(ctx context.Context, task provider.UploadTask, partSize int64, streams int) (provider.UploadResult, error) {
		select {
		case called <- streams:
		default:
		}
		return provider.UploadResult{RemotePath: task.RemotePath, ChecksumAlgo: "sha256", Checksum: "deadbeef"}, nil
	}

	body, contentType := multipartUpload(t, "m.txt", "/m.txt", []byte("payload"))
	if rec := env.serve(env.newUploadRequest(conn.ID, body, contentType)); rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, want 202", rec.Code)
	}

	select {
	case streams := <-called:
		if streams != 2 {
			t.Errorf("UploadMultipartParallel called with streams = %d, want 2", streams)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UploadMultipartParallel was never called - multi-thread-streams settings did not reach the queue")
	}
}

// TestSettingsPutAppliesUploadBandwidthLimitToTheLiveQueue - the fourth
// setting queue.Manager can apply live. 1500 bytes at a 1000 bytes/sec cap
// (burst == rate, so the first 1000 bytes are free) must take at least
// ~400ms to drain through task.Reader if the limit actually reached the
// queue's shared rate.Limiter.
func TestSettingsPutAppliesUploadBandwidthLimitToTheLiveQueue(t *testing.T) {
	env := newTestEnv(t)

	if rec := env.doJSON(http.MethodPut, "/api/v1/settings", settings.Settings{
		MaxConcurrentUploadsPerProvider: 1,
		Language:                        "en",
		MaxUploadBytesPerSecond:         1000,
	}); rec.Code != http.StatusOK {
		t.Fatalf("PUT settings status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	conn := env.createConnection(fakeType, "Throttled", map[string]string{"url": "u"}, nil)

	elapsed := make(chan time.Duration, 1)
	behaviorFor(t, conn.ID).upload = func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		start := time.Now()
		if _, err := io.Copy(io.Discard, task.Reader); err != nil {
			return provider.UploadResult{}, err
		}
		select {
		case elapsed <- time.Since(start):
		default:
		}
		return provider.UploadResult{RemotePath: task.RemotePath, ChecksumAlgo: "sha256", Checksum: "deadbeef"}, nil
	}

	data := bytes.Repeat([]byte("a"), 1500)
	body, contentType := multipartUpload(t, "bw.txt", "/bw.txt", data)
	if rec := env.serve(env.newUploadRequest(conn.ID, body, contentType)); rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, want 202", rec.Code)
	}

	select {
	case d := <-elapsed:
		const wantMin = 400 * time.Millisecond
		if d < wantMin {
			t.Errorf("upload of 1500 bytes at 1000 bytes/sec took %s, want at least %s - upload bandwidth limit did not reach the queue", d, wantMin)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload was never observed completing")
	}
}
