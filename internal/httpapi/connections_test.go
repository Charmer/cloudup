package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloudup/internal/provider"
	"cloudup/internal/registry"
)

// TestProviderTypesReturnsObjectsWithRequiresOAuth pins the response
// *shape* of GET /api/v1/provider-types, which openapi.yaml documents as an
// array of objects. It used to be a bare array of strings; a client built
// against the spec breaks silently if it regresses, and no handler-level
// assertion would notice.
func TestProviderTypesReturnsObjectsWithRequiresOAuth(t *testing.T) {
	env := newTestEnv(t)

	views := decodeBody[[]providerTypeView](t, env.do(http.MethodGet, "/api/v1/provider-types", nil), http.StatusOK)
	if len(views) == 0 {
		t.Fatal("no provider types returned")
	}

	got := map[string]bool{}
	for _, v := range views {
		if v.Type == "" {
			t.Errorf("entry with empty type: %+v", v)
		}
		got[v.Type] = v.RequiresOAuth
	}

	// The real providers: requiresOAuth is true for exactly the two types
	// whose connections need an interactive consent step, false for the
	// key/password-configured ones. Hardcoded on purpose - this is the
	// assertion a client would otherwise have to make itself.
	want := map[string]bool{
		"webdav":      false,
		"s3":          false,
		"b2":          false,
		"googledrive": true,
		"dropbox":     true,
	}
	for providerType, wantOAuth := range want {
		gotOAuth, ok := got[providerType]
		if !ok {
			t.Errorf("provider type %q missing from the response", providerType)
			continue
		}
		if gotOAuth != wantOAuth {
			t.Errorf("provider type %q requiresOAuth = %v, want %v", providerType, gotOAuth, wantOAuth)
		}
	}

	// And the response must agree with the registry for every type,
	// including any added later without touching this test.
	for _, providerType := range registry.Types() {
		gotOAuth, ok := got[providerType]
		if !ok {
			t.Errorf("registered provider type %q missing from GET /provider-types", providerType)
			continue
		}
		if gotOAuth != registry.RequiresOAuth(providerType) {
			t.Errorf("provider type %q requiresOAuth = %v, want %v (registry)",
				providerType, gotOAuth, registry.RequiresOAuth(providerType))
		}
	}
}

// TestProviderSchemaReturnsFieldSpecs checks the schema endpoint hands back
// the provider's field specs (the form definition a client renders) and
// 404s an unknown type rather than an empty list, which a client could not
// distinguish from "this provider needs no configuration".
func TestProviderSchemaReturnsFieldSpecs(t *testing.T) {
	env := newTestEnv(t)

	fields := decodeBody[[]provider.FieldSpec](t,
		env.do(http.MethodGet, "/api/v1/provider-types/"+fakeType+"/schema", nil), http.StatusOK)
	if len(fields) != 3 {
		t.Fatalf("schema fields = %+v, want 3", fields)
	}
	byKey := map[string]provider.FieldSpec{}
	for _, f := range fields {
		byKey[f.Key] = f
	}
	if byKey["url"].Type != provider.FieldText || !byKey["url"].Required {
		t.Errorf("url field = %+v, want a required text field", byKey["url"])
	}
	if byKey["password"].Type != provider.FieldPassword {
		t.Errorf("password field = %+v, want type password", byKey["password"])
	}
	if len(byKey["mode"].Options) != 2 {
		t.Errorf("mode field = %+v, want two select options", byKey["mode"])
	}

	msg := errorMessage(t, env.do(http.MethodGet, "/api/v1/provider-types/no-such-type/schema", nil), http.StatusNotFound)
	if !strings.Contains(msg, "no-such-type") {
		t.Errorf("error = %q, want it to name the unknown type", msg)
	}
}

// TestConnectionCRUDRoundTrip walks create -> list -> get -> update ->
// delete through the routed handler, asserting the connectionView shape at
// each step.
func TestConnectionCRUDRoundTrip(t *testing.T) {
	env := newTestEnv(t)

	created := env.createConnection(fakeType, "My Storage",
		map[string]string{"url": "https://example.invalid", "mode": "a"},
		map[string]string{"password": "s3cr3t"})

	if created.ID == "" {
		t.Fatal("created connection has no ID")
	}
	if created.ProviderType != fakeType || created.DisplayName != "My Storage" {
		t.Errorf("created = %+v, want the submitted type/name", created)
	}
	if created.Fields["url"] != "https://example.invalid" || created.Fields["mode"] != "a" {
		t.Errorf("created.Fields = %v, want the submitted fields", created.Fields)
	}
	if created.CreatedAt.IsZero() {
		t.Error("created.CreatedAt is zero")
	}

	list := decodeBody[[]connectionView](t, env.do(http.MethodGet, "/api/v1/connections", nil), http.StatusOK)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want the one created connection", list)
	}

	got := decodeBody[connectionView](t, env.do(http.MethodGet, "/api/v1/connections/"+created.ID, nil), http.StatusOK)
	if got.ID != created.ID || got.Fields["url"] != created.Fields["url"] {
		t.Errorf("get = %+v, want %+v", got, created)
	}

	updated := decodeBody[connectionView](t, env.doJSON(http.MethodPut, "/api/v1/connections/"+created.ID,
		connectionRequest{DisplayName: "Renamed", Fields: map[string]string{"url": "https://other.invalid"}}),
		http.StatusOK)
	if updated.ID != created.ID {
		t.Errorf("update changed the ID: %q -> %q", created.ID, updated.ID)
	}
	if updated.DisplayName != "Renamed" || updated.Fields["url"] != "https://other.invalid" {
		t.Errorf("update = %+v, want the new name/fields", updated)
	}
	if updated.ProviderType != fakeType {
		t.Errorf("update changed providerType to %q", updated.ProviderType)
	}
	if _, stale := updated.Fields["mode"]; stale {
		t.Errorf("update kept a field that was not resubmitted: %v", updated.Fields)
	}

	if rec := env.do(http.MethodDelete, "/api/v1/connections/"+created.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := env.do(http.MethodGet, "/api/v1/connections/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete status = %d, want 404", rec.Code)
	}
	list = decodeBody[[]connectionView](t, env.do(http.MethodGet, "/api/v1/connections", nil), http.StatusOK)
	if len(list) != 0 {
		t.Errorf("list after delete = %+v, want empty", list)
	}
}

// TestDeleteConnectionWithActiveUploadIsRejected covers the guard added
// alongside queue.Manager's idle-queue sweep: deleting a connection while
// it still has a pending or in-flight upload must fail with 409 rather
// than let that upload keep running against a config that no longer
// exists (see handleConnectionsDelete's doc comment). Once the upload
// finishes, the same delete must succeed.
func TestDeleteConnectionWithActiveUploadIsRejected(t *testing.T) {
	env := newTestEnv(t)

	conn := env.createConnection(fakeType, "Busy", map[string]string{"url": "u"}, nil)

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	behaviorFor(t, conn.ID).upload = func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return provider.UploadResult{RemotePath: task.RemotePath, ChecksumAlgo: "sha256", Checksum: "abc"}, nil
	}

	body, contentType := multipartUpload(t, "hello.txt", "/remote/hello.txt", []byte("x"))
	env.serve(env.newUploadRequest(conn.ID, body, contentType))

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the provider's Upload was never called")
	}

	rec := env.do(http.MethodDelete, "/api/v1/connections/"+conn.ID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete status while upload is in flight = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	// Rejected, not partially applied - the connection must still exist.
	if rec := env.do(http.MethodGet, "/api/v1/connections/"+conn.ID, nil); rec.Code != http.StatusOK {
		t.Errorf("get after rejected delete status = %d, want 200 (connection should be untouched)", rec.Code)
	}

	close(release)
	waitFor(t, 3*time.Second, "the upload to finish so the connection has no active tasks", func() bool {
		return !env.Server.Queue.HasActiveTasks(conn.ID)
	})

	if rec := env.do(http.MethodDelete, "/api/v1/connections/"+conn.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status after upload finished = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestSecretsNeverEnterTheConnectionConfig is the security-critical
// invariant of this package: a value submitted under "secrets" goes to the
// secret store and nowhere else. It must not appear in the response, in
// config.json, or in the stored ProviderConfig.
func TestSecretsNeverEnterTheConnectionConfig(t *testing.T) {
	env := newTestEnv(t)
	const password = "correct-horse-battery-staple"

	created := env.createConnection(fakeType, "Secretive",
		map[string]string{"url": "https://example.invalid"},
		map[string]string{"password": password})

	if _, leaked := created.Fields["password"]; leaked {
		t.Errorf("create response leaked the secret in fields: %v", created.Fields)
	}

	conn, err := env.Config.Get(created.ID)
	if err != nil {
		t.Fatalf("config.Get() error = %v", err)
	}
	if strings.Contains(string(conn.ProviderConfig), password) {
		t.Errorf("secret value found in the persisted provider config: %s", conn.ProviderConfig)
	}

	stored, err := env.Secrets.Get(created.ID, "password")
	if err != nil {
		t.Fatalf("secrets.Get() error = %v", err)
	}
	if stored != password {
		t.Errorf("stored secret = %q, want %q", stored, password)
	}

	// The same rule on update.
	const rotated = "rotated-secret"
	view := decodeBody[connectionView](t, env.doJSON(http.MethodPut, "/api/v1/connections/"+created.ID,
		connectionRequest{DisplayName: "Secretive", Fields: map[string]string{"url": "https://example.invalid"},
			Secrets: map[string]string{"password": rotated}}), http.StatusOK)
	if _, leaked := view.Fields["password"]; leaked {
		t.Errorf("update response leaked the secret in fields: %v", view.Fields)
	}
	if stored, _ := env.Secrets.Get(created.ID, "password"); stored != rotated {
		t.Errorf("stored secret after update = %q, want %q", stored, rotated)
	}
	conn, _ = env.Config.Get(created.ID)
	if strings.Contains(string(conn.ProviderConfig), rotated) {
		t.Errorf("rotated secret found in the persisted provider config: %s", conn.ProviderConfig)
	}

	t.Cleanup(func() { _ = env.Secrets.Delete(created.ID, "password") })
}

// TestConnectionViewHidesInternalConnectionIDField: config.Store injects a
// "connectionId" key into every ProviderConfig for the provider factory to
// read back. It is internal plumbing and must not surface as a form field a
// client would render or echo back on update.
func TestConnectionViewHidesInternalConnectionIDField(t *testing.T) {
	env := newTestEnv(t)

	created := env.createConnection(fakeType, "Hidden", map[string]string{"url": "u"}, nil)

	if _, leaked := created.Fields["connectionId"]; leaked {
		t.Errorf("create response exposes connectionId in fields: %v", created.Fields)
	}

	// It *is* still in the stored config - the point is that the view hides
	// it, not that it stopped existing.
	conn, err := env.Config.Get(created.ID)
	if err != nil {
		t.Fatalf("config.Get() error = %v", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(conn.ProviderConfig, &raw); err != nil {
		t.Fatalf("unmarshalling provider config: %v", err)
	}
	if raw["connectionId"] != created.ID {
		t.Errorf("stored connectionId = %q, want %q", raw["connectionId"], created.ID)
	}

	got := decodeBody[connectionView](t, env.do(http.MethodGet, "/api/v1/connections/"+created.ID, nil), http.StatusOK)
	if _, leaked := got.Fields["connectionId"]; leaked {
		t.Errorf("get response exposes connectionId in fields: %v", got.Fields)
	}
	list := decodeBody[[]connectionView](t, env.do(http.MethodGet, "/api/v1/connections", nil), http.StatusOK)
	if _, leaked := list[0].Fields["connectionId"]; leaked {
		t.Errorf("list response exposes connectionId in fields: %v", list[0].Fields)
	}
}

// TestDeleteConnectionRemovesItsSecrets covers the cleanup path: every
// FieldPassword key from the provider's schema is deleted from the secret
// store when the connection goes away, so a later connection reusing an ID
// (or a curious keychain reader) cannot find them.
func TestDeleteConnectionRemovesItsSecrets(t *testing.T) {
	env := newTestEnv(t)

	created := env.createConnection(fakeType, "Doomed",
		map[string]string{"url": "u"}, map[string]string{"password": "to-be-deleted"})

	if stored, _ := env.Secrets.Get(created.ID, "password"); stored == "" {
		t.Fatal("secret was not stored to begin with")
	}
	if rec := env.do(http.MethodDelete, "/api/v1/connections/"+created.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	stored, err := env.Secrets.Get(created.ID, "password")
	if err != nil {
		t.Fatalf("secrets.Get() error = %v", err)
	}
	if stored != "" {
		t.Errorf("secret survived the connection delete: %q", stored)
	}
}

// TestDeleteOAuthConnectionRemovesRefreshToken is a regression test for a
// recently fixed bug: cleanup only removed the schema's FieldPassword keys,
// so an OAuth provider's refresh token - which is never a schema field -
// stayed in the keychain after its connection was deleted.
func TestDeleteOAuthConnectionRemovesRefreshToken(t *testing.T) {
	env := newTestEnv(t)

	created := env.createConnection(fakeOAuthType, "Drive-ish", map[string]string{"url": "u"}, nil)

	flow, ok := registry.OAuth(fakeOAuthType)
	if !ok {
		t.Fatalf("no OAuth flow registered for %q", fakeOAuthType)
	}
	if err := env.Secrets.Set(created.ID, flow.RefreshTokenKey, "a-refresh-token"); err != nil {
		t.Fatalf("seeding refresh token: %v", err)
	}

	if rec := env.do(http.MethodDelete, "/api/v1/connections/"+created.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}

	stored, err := env.Secrets.Get(created.ID, flow.RefreshTokenKey)
	if err != nil {
		t.Fatalf("secrets.Get() error = %v", err)
	}
	if stored != "" {
		t.Errorf("refresh token survived the connection delete: %q", stored)
	}
}

// TestConnectionCreateValidatesRequiredBodyFields - the two fields the
// handler rejects up front, plus a body that is not JSON at all.
func TestConnectionCreateValidatesRequiredBodyFields(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name string
		body connectionRequest
	}{
		{"no provider type", connectionRequest{DisplayName: "x"}},
		{"no display name", connectionRequest{ProviderType: fakeType}},
		{"neither", connectionRequest{}},
	}
	for _, tc := range cases {
		rec := env.doJSON(http.MethodPost, "/api/v1/connections", tc.body)
		if msg := errorMessage(t, rec, http.StatusBadRequest); msg == "" {
			t.Errorf("%s: empty error message", tc.name)
		}
	}

	rec := env.do(http.MethodPost, "/api/v1/connections", strings.NewReader("not json"))
	errorMessage(t, rec, http.StatusBadRequest)

	// Unknown fields are rejected rather than silently ignored (the
	// decoder uses DisallowUnknownFields), so a client typo cannot look
	// like a successful create that lost data.
	rec = env.do(http.MethodPost, "/api/v1/connections",
		strings.NewReader(`{"providerType":"`+fakeType+`","displayName":"x","feilds":{}}`))
	errorMessage(t, rec, http.StatusBadRequest)
}

// TestConnectionEndpointsReport404ForUnknownID: every per-connection route
// must answer a JSON 404 for an ID that does not exist, not a mux 404 or a
// 500.
func TestConnectionEndpointsReport404ForUnknownID(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodGet, "/api/v1/connections/ghost", nil)
	errorMessage(t, rec, http.StatusNotFound)

	rec = env.doJSON(http.MethodPut, "/api/v1/connections/ghost", connectionRequest{DisplayName: "x"})
	errorMessage(t, rec, http.StatusNotFound)

	rec = env.do(http.MethodDelete, "/api/v1/connections/ghost", nil)
	errorMessage(t, rec, http.StatusNotFound)

	rec = env.do(http.MethodPost, "/api/v1/connections/ghost/test", nil)
	errorMessage(t, rec, http.StatusNotFound)
}

// TestConnectionTestReportsProviderOutcomeAs200 pins a shape that is easy
// to get wrong: a *failed* connection test is a successful API call
// reporting {"ok":false,"error":...}, not an HTTP error - the client needs
// to show the message, not treat it as a transport failure.
func TestConnectionTestReportsProviderOutcomeAs200(t *testing.T) {
	env := newTestEnv(t)

	created := env.createConnection(fakeType, "Testable", map[string]string{"url": "u"}, nil)

	body := decodeBody[map[string]any](t, env.do(http.MethodPost, "/api/v1/connections/"+created.ID+"/test", nil), http.StatusOK)
	if body["ok"] != true {
		t.Errorf("test body = %v, want ok true", body)
	}

	behaviorFor(t, created.ID).testConn = errTestConnection
	body = decodeBody[map[string]any](t, env.do(http.MethodPost, "/api/v1/connections/"+created.ID+"/test", nil), http.StatusOK)
	if body["ok"] != false {
		t.Errorf("failing test body = %v, want ok false", body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, errTestConnection.Error()) {
		t.Errorf("failing test body = %v, want the provider's error message", body)
	}
}

// TestConnectionTestReportsUnconstructableProviderAs400: a connection whose
// provider cannot be built at all (the real-world case: OAuth credentials
// not configured yet) is a client-fixable 400, distinct from the 200
// "ok:false" above.
func TestConnectionTestReportsUnconstructableProviderAs400(t *testing.T) {
	env := newTestEnv(t)

	created := env.createConnection(fakeType, "Broken",
		map[string]string{"url": "u", "fail": "credentials missing"}, nil)

	msg := errorMessage(t, env.do(http.MethodPost, "/api/v1/connections/"+created.ID+"/test", nil), http.StatusBadRequest)
	if !strings.Contains(msg, "credentials missing") {
		t.Errorf("error = %q, want the factory's message", msg)
	}
}
