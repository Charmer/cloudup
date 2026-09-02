// Test support for the httpapi package.
//
// Everything here exists so the tests can drive the *real* routed handler
// (Server.Handler) end to end - routing, auth middleware, CORS, path
// values, JSON encoding included - instead of calling handler methods
// directly. Wiring bugs (a route that was never registered, a provider
// type whose OAuth flow is not reachable) only show up when the request
// goes through the mux, which is exactly the class of bug these tests are
// here to catch.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"cloudup/internal/config"
	"cloudup/internal/history"
	"cloudup/internal/provider"
	"cloudup/internal/queue"
	"cloudup/internal/registry"
	"cloudup/internal/secrets"
	"cloudup/internal/settings"
	"cloudup/internal/watch"

	// The real providers are imported for their init()-time
	// registry.Register/RegisterSchema/RegisterOAuth side effects only.
	// Almost every test uses the purpose-built fake type below instead, but
	// two invariants are *about* the real ones: that GET /provider-types
	// reports requiresOAuth correctly for them, and that every type
	// claiming requiresOAuth has its flow actually reachable through the
	// OAuth endpoints (see oauth_test.go's registry loop - the Dropbox
	// "flow existed but was wired to nothing" bug lived precisely there).
	_ "cloudup/internal/providers/b2"
	_ "cloudup/internal/providers/dropbox"
	_ "cloudup/internal/providers/googledrive"
	_ "cloudup/internal/providers/s3"
	_ "cloudup/internal/providers/webdav"
)

// TestMain switches go-keyring to its in-memory mock, exactly as
// internal/secrets' own tests do: these tests store connection secrets and
// OAuth client credentials, and must never write into the developer's real
// OS keychain.
// It also silences withLogging's per-request log lines, which would
// otherwise bury test output; nothing here asserts on them.
func TestMain(m *testing.M) {
	keyring.MockInit()
	log.SetOutput(io.Discard)
	m.Run()
}

// Test-only provider types. registry.Register panics on a duplicate
// registration, so these names are deliberately unlike any real provider's
// and are registered exactly once, from init below.
const (
	fakeType      = "httpapi-test-fake"
	fakeOAuthType = "httpapi-test-oauth"

	// fakeOAuthAppID is where the fake flow's app-wide client credentials
	// live in the secret store (an OAuthFlow.AppCredentialsID).
	fakeOAuthAppID = "httpapi-test-oauth-app"
)

// fakeBehavior lets a test steer the fake provider built for one specific
// connection. Keyed by connection ID (which config.Store injects into
// every ProviderConfig), so two connections in the same test can behave
// differently.
type fakeBehavior struct {
	// upload, if set, replaces the default "drain the body, report a
	// checksum" behavior.
	upload func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error)
	// testConn is what TestConnection returns.
	testConn error
	// exists/verify back the optional ExistenceChecker/ChecksumVerifier
	// interfaces history verification looks for.
	exists func(ctx context.Context, remotePath string) (bool, error)
	verify func(ctx context.Context, remotePath, algo, checksum string) (bool, error)
	// parallelMultipart, if set, replaces the default UploadMultipartParallel
	// behavior (see fakeProvider.UploadMultipartParallel). fakeProvider
	// always implements provider.ParallelMultipartUploader structurally
	// (Go has no way to opt a single value in/out of an interface at
	// runtime); what actually matters for these tests is that dispatch to
	// it only happens when a test explicitly lowers
	// MultiThreadThresholdBytes far below the real 256 MiB default, so
	// every other test's tiny payloads never come near it.
	parallelMultipart func(ctx context.Context, task provider.UploadTask, partSize int64, streams int) (provider.UploadResult, error)
}

var (
	fakeMu        sync.Mutex
	fakeBehaviors = map[string]*fakeBehavior{}
)

// behaviorFor returns (creating if needed) the behavior hook for one
// connection ID. Tests mutate the returned struct before triggering the
// request that will construct the provider.
func behaviorFor(t *testing.T, connectionID string) *fakeBehavior {
	t.Helper()
	fakeMu.Lock()
	defer fakeMu.Unlock()
	b, ok := fakeBehaviors[connectionID]
	if !ok {
		b = &fakeBehavior{}
		fakeBehaviors[connectionID] = b
	}
	// Connection IDs are freshly generated per test, so leftovers would
	// only grow the map; drop this one when the test ends.
	t.Cleanup(func() {
		fakeMu.Lock()
		delete(fakeBehaviors, connectionID)
		fakeMu.Unlock()
	})
	return b
}

func lookupBehavior(connectionID string) *fakeBehavior {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	if b, ok := fakeBehaviors[connectionID]; ok {
		return b
	}
	return &fakeBehavior{}
}

// fakeProvider implements provider.Provider plus the two optional feature
// interfaces history verification probes for.
type fakeProvider struct {
	connectionID string
	typeName     string
}

func (p fakeProvider) Type() string        { return p.typeName }
func (p fakeProvider) DisplayName() string { return p.typeName }

func (p fakeProvider) TestConnection(ctx context.Context) error {
	return lookupBehavior(p.connectionID).testConn
}

func (p fakeProvider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	if fn := lookupBehavior(p.connectionID).upload; fn != nil {
		return fn(ctx, task)
	}
	n, err := io.Copy(io.Discard, task.Reader)
	if err != nil {
		return provider.UploadResult{}, err
	}
	if task.Progress != nil {
		task.Progress(n, task.Size)
	}
	return provider.UploadResult{RemotePath: task.RemotePath, ChecksumAlgo: "sha256", Checksum: "deadbeef"}, nil
}

// UploadMultipartParallel makes fakeProvider satisfy
// provider.ParallelMultipartUploader unconditionally - see
// fakeBehavior.parallelMultipart's doc comment for why that is safe for
// every test that never opts into it via a very low
// MultiThreadThresholdBytes.
func (p fakeProvider) UploadMultipartParallel(ctx context.Context, task provider.UploadTask, partSize int64, streams int) (provider.UploadResult, error) {
	if fn := lookupBehavior(p.connectionID).parallelMultipart; fn != nil {
		return fn(ctx, task, partSize, streams)
	}
	n, err := io.Copy(io.Discard, task.Reader)
	if err != nil {
		return provider.UploadResult{}, err
	}
	if task.Progress != nil {
		task.Progress(n, task.Size)
	}
	return provider.UploadResult{RemotePath: task.RemotePath, ChecksumAlgo: "sha256", Checksum: "deadbeef"}, nil
}

func (p fakeProvider) Download(ctx context.Context, task provider.DownloadTask) error { return nil }

func (p fakeProvider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	return nil, nil
}

func (p fakeProvider) Delete(ctx context.Context, remotePath string) error { return nil }

func (p fakeProvider) Exists(ctx context.Context, remotePath string) (bool, error) {
	if fn := lookupBehavior(p.connectionID).exists; fn != nil {
		return fn(ctx, remotePath)
	}
	return true, nil
}

func (p fakeProvider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if fn := lookupBehavior(p.connectionID).verify; fn != nil {
		return fn(ctx, remotePath, algo, checksum)
	}
	return true, nil
}

// fakeAuthURLFn/fakeExchangeFn are the fake OAuth flow's AuthURL/Exchange
// implementations. A test swaps either via setFakeAuthURL/setFakeExchange;
// the defaults report a deterministic-but-unique consent URL/state and
// hand back a refresh token immediately.
var (
	fakeOAuthMu    sync.Mutex
	fakeAuthURLFn  = defaultFakeAuthURL
	fakeExchangeFn = defaultFakeExchange
	fakeStateSeq   atomic.Int64
)

func defaultFakeAuthURL(params provider.AuthURLParams) (string, string, error) {
	state := fmt.Sprintf("fake-state-%d", fakeStateSeq.Add(1))
	return "https://example.invalid/consent?client_id=" + params.ClientID + "&state=" + state, state, nil
}

func defaultFakeExchange(ctx context.Context, params provider.ExchangeParams, code string) (string, error) {
	return "refresh-token-for-" + params.ClientID, nil
}

func setFakeAuthURL(t *testing.T, fn func(provider.AuthURLParams) (string, string, error)) {
	t.Helper()
	fakeOAuthMu.Lock()
	fakeAuthURLFn = fn
	fakeOAuthMu.Unlock()
	t.Cleanup(func() {
		fakeOAuthMu.Lock()
		fakeAuthURLFn = defaultFakeAuthURL
		fakeOAuthMu.Unlock()
	})
}

func setFakeExchange(t *testing.T, fn func(ctx context.Context, params provider.ExchangeParams, code string) (string, error)) {
	t.Helper()
	fakeOAuthMu.Lock()
	fakeExchangeFn = fn
	fakeOAuthMu.Unlock()
	t.Cleanup(func() {
		fakeOAuthMu.Lock()
		fakeExchangeFn = defaultFakeExchange
		fakeOAuthMu.Unlock()
	})
}

func init() {
	newFake := func(typeName string) registry.Factory {
		return func(cfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
			var raw struct {
				ConnectionID string `json:"connectionId"`
				Fail         string `json:"fail"`
			}
			if err := json.Unmarshal(cfg, &raw); err != nil {
				return nil, fmt.Errorf("%s: invalid config: %w", typeName, err)
			}
			// A "fail" field gives tests a connection whose provider
			// cannot be constructed - the shape of every real provider's
			// "credentials missing" failure, without needing a real one.
			if raw.Fail != "" {
				return nil, fmt.Errorf("%s: cannot construct provider: %s", typeName, raw.Fail)
			}
			return fakeProvider{connectionID: raw.ConnectionID, typeName: typeName}, nil
		}
	}
	schema := func() []provider.FieldSpec {
		return []provider.FieldSpec{
			{Key: "url", Label: "URL", Type: provider.FieldText, Required: true},
			{Key: "password", Label: "Password", Type: provider.FieldPassword},
			{Key: "mode", Label: "Mode", Type: provider.FieldSelect, Options: []string{"a", "b"}},
		}
	}

	registry.Register(fakeType, newFake(fakeType))
	registry.RegisterSchema(fakeType, schema)

	registry.Register(fakeOAuthType, newFake(fakeOAuthType))
	registry.RegisterSchema(fakeOAuthType, schema)
	registry.RegisterOAuth(fakeOAuthType, provider.OAuthFlow{
		AppCredentialsID: fakeOAuthAppID,
		ClientIDKey:      "clientId",
		ClientSecretKey:  "clientSecret",
		RefreshTokenKey:  "refreshToken",
		AuthURL: func(params provider.AuthURLParams) (string, string, error) {
			fakeOAuthMu.Lock()
			fn := fakeAuthURLFn
			fakeOAuthMu.Unlock()
			return fn(params)
		},
		Exchange: func(ctx context.Context, params provider.ExchangeParams, code string) (string, error) {
			fakeOAuthMu.Lock()
			fn := fakeExchangeFn
			fakeOAuthMu.Unlock()
			return fn(ctx, params, code)
		},
	})
}

// testEnv is one fully wired Server plus its handler and the temp-backed
// stores behind it. Every store is per-test (temp dir), so tests never see
// each other's connections, history rows or settings. The keychain is the
// one shared resource (mocked process-wide), which is why secret-touching
// tests clean up after themselves.
type testEnv struct {
	t *testing.T

	Server  *Server
	Handler http.Handler

	Config   *config.Store
	Secrets  *secrets.Store
	History  *history.Store
	Settings *settings.Store
	Queue    *queue.Manager

	Token       string
	SpoolDir    string
	OpenAPIPath string
	StaticDir   string
}

func newTestEnv(t *testing.T, opts ...func(*Server)) *testEnv {
	t.Helper()

	dir := t.TempDir()

	cfg, err := config.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config.Open() error = %v", err)
	}
	hist, err := history.Open(filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open() error = %v", err)
	}
	t.Cleanup(func() { hist.Close() })
	set, err := settings.Open(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("settings.Open() error = %v", err)
	}

	sec := secrets.New()
	mgr := queue.NewManager(hist, queue.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	})

	watchStore, err := watch.Open(filepath.Join(dir, "watches.json"))
	if err != nil {
		t.Fatalf("watch.Open() error = %v", err)
	}

	spoolDir := filepath.Join(dir, "spool")
	const token = "test-token"

	srv := NewServer(cfg, sec, hist, set, mgr, watchStore, token, spoolDir)
	t.Cleanup(func() {
		if srv.WatchEngine != nil {
			srv.WatchEngine.Shutdown()
		}
	})
	for _, opt := range opts {
		opt(srv)
	}

	openapiPath := filepath.Join(dir, "openapi.yaml")
	writeFile(t, openapiPath, "openapi: 3.0.3\n")

	return &testEnv{
		t:           t,
		Server:      srv,
		Handler:     srv.Handler(openapiPath),
		Config:      cfg,
		Secrets:     sec,
		History:     hist,
		Settings:    set,
		Queue:       mgr,
		Token:       token,
		SpoolDir:    spoolDir,
		OpenAPIPath: openapiPath,
	}
}

// do sends a request through the full handler with a valid bearer token.
func (e *testEnv) do(method, target string, body io.Reader) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+e.Token)
	return e.serve(req)
}

// doJSON sends a JSON body through the full handler with a valid token.
func (e *testEnv) doJSON(method, target string, payload any) *httptest.ResponseRecorder {
	e.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatalf("marshalling request body: %v", err)
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+e.Token)
	req.Header.Set("Content-Type", "application/json")
	return e.serve(req)
}

func (e *testEnv) serve(req *http.Request) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.Handler.ServeHTTP(rec, req)
	return rec
}

// decode unmarshals a recorded JSON response, failing the test on a
// status mismatch or unparseable body.
func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) T {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding response: %v (body: %s)", err, rec.Body.String())
	}
	return v
}

// errorMessage pulls the "error" string out of an error response, asserting
// the documented {"error": "..."} envelope every failing endpoint uses.
func errorMessage(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) string {
	t.Helper()
	body := decodeBody[map[string]string](t, rec, wantStatus)
	msg, ok := body["error"]
	if !ok {
		t.Fatalf("error response has no \"error\" field: %s", rec.Body.String())
	}
	return msg
}

// createConnection creates a connection of providerType through the API
// (not through config.Store directly) so the create path is exercised and
// the returned view is the API's own.
func (e *testEnv) createConnection(providerType, displayName string, fields, secretValues map[string]string) connectionView {
	e.t.Helper()
	rec := e.doJSON(http.MethodPost, "/api/v1/connections", connectionRequest{
		ProviderType: providerType,
		DisplayName:  displayName,
		Fields:       fields,
		Secrets:      secretValues,
	})
	return decodeBody[connectionView](e.t, rec, http.StatusCreated)
}

// newUploadRequest builds an authenticated POST to a connection's uploads
// endpoint with the given body and content type.
func (e *testEnv) newUploadRequest(connectionID string, body io.Reader, contentType string) *http.Request {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connectionID+"/uploads", body)
	req.Header.Set("Authorization", "Bearer "+e.Token)
	req.Header.Set("Content-Type", contentType)
	return req
}

// multipartUpload builds a multipart/form-data upload request body the way
// a browser's FormData would.
func multipartUpload(t *testing.T, filename, remotePath string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if remotePath != "" {
		if err := mw.WriteField("remotePath", remotePath); err != nil {
			t.Fatalf("WriteField() error = %v", err)
		}
	}
	if filename != "" {
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("CreateFormFile() error = %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("writing file part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// waitFor polls cond until it holds or the timeout expires. Polling rather
// than sleeping keeps the async task tests from being flaky, matching how
// internal/queue's tests wait on events.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// itoa keeps history ID path building readable.
func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// errTestConnection is the canned failure a fake provider's TestConnection
// can return.
var errTestConnection = errors.New("fake provider: cannot reach the remote")

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
