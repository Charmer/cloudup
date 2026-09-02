package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeGitHubClient returns an *http.Client whose RoundTripper rewrites
// every request's host to fake's, so tests never reach real GitHub -
// handleUpdatesCheck otherwise hardcodes api.github.com through
// internal/updatecheck.
func fakeGitHubClient(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &http.Client{Transport: rewriteHostTransport{targetURL: srv.URL}}
}

type rewriteHostTransport struct {
	targetURL string
}

func (rt rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := http.NewRequest(req.Method, rt.targetURL+req.URL.Path, req.Body)
	if err != nil {
		return nil, err
	}
	target.Header = req.Header
	return http.DefaultTransport.RoundTrip(target)
}

func TestUpdatesCheckReportsUpdateAvailable(t *testing.T) {
	client := fakeGitHubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/someone/cloudup/releases/latest" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]string{
			"tag_name": "v2.0.0",
			"html_url": "https://github.com/someone/cloudup/releases/tag/v2.0.0",
		})
	})

	env := newTestEnv(t, func(s *Server) {
		s.Version = "v1.0.0"
		s.UpdateRepo = "someone/cloudup"
		s.updateHTTPClient = client
	})

	got := decodeBody[updateCheckResponse](t, env.do(http.MethodGet, "/api/v1/updates/check", nil), http.StatusOK)
	want := updateCheckResponse{
		CurrentVersion:  "v1.0.0",
		LatestVersion:   "v2.0.0",
		UpdateAvailable: true,
		ReleaseURL:      "https://github.com/someone/cloudup/releases/tag/v2.0.0",
	}
	if got != want {
		t.Errorf("response = %+v, want %+v", got, want)
	}
}

func TestUpdatesCheckUpToDate(t *testing.T) {
	client := fakeGitHubClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"tag_name": "v1.0.0", "html_url": "https://github.com/someone/cloudup/releases/tag/v1.0.0"})
	})

	env := newTestEnv(t, func(s *Server) {
		s.Version = "v1.0.0"
		s.UpdateRepo = "someone/cloudup"
		s.updateHTTPClient = client
	})

	got := decodeBody[updateCheckResponse](t, env.do(http.MethodGet, "/api/v1/updates/check", nil), http.StatusOK)
	if got.UpdateAvailable {
		t.Errorf("UpdateAvailable = true, want false when already on the latest version: %+v", got)
	}
}

func TestUpdatesCheckDevBuildNeverClaimsAnUpdate(t *testing.T) {
	client := fakeGitHubClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"tag_name": "v1.0.0", "html_url": "https://github.com/someone/cloudup/releases/tag/v1.0.0"})
	})

	env := newTestEnv(t, func(s *Server) {
		s.Version = "dev"
		s.UpdateRepo = "someone/cloudup"
		s.updateHTTPClient = client
	})

	got := decodeBody[updateCheckResponse](t, env.do(http.MethodGet, "/api/v1/updates/check", nil), http.StatusOK)
	if got.UpdateAvailable {
		t.Errorf("UpdateAvailable = true for an unversioned dev build, want false: %+v", got)
	}
	if got.LatestVersion != "v1.0.0" {
		t.Errorf("LatestVersion = %q, want it reported regardless of comparability", got.LatestVersion)
	}
}

func TestUpdatesCheckUpstreamErrorIsBadGateway(t *testing.T) {
	client := fakeGitHubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	env := newTestEnv(t, func(s *Server) {
		s.Version = "v1.0.0"
		s.UpdateRepo = "someone/cloudup"
		s.updateHTTPClient = client
	})

	env.do(http.MethodGet, "/api/v1/updates/check", nil)
	rec := env.do(http.MethodGet, "/api/v1/updates/check", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestUpdatesCheckRejectsMalformedUpdateRepo(t *testing.T) {
	env := newTestEnv(t, func(s *Server) {
		s.Version = "v1.0.0"
		s.UpdateRepo = "not-an-owner-slash-repo"
	})

	rec := env.do(http.MethodGet, "/api/v1/updates/check", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestUpdatesCheckRequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/updates/check", nil)
	rec := env.serve(req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
