package updatecheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.0", "v1.1.0", true},
		{"v1.0.0", "v2.0.0", true},
		{"1.0.0", "v1.0.0", false},  // no "v" prefix on current, same version
		{"v1.2.3", "v1.2.3", false}, // equal
		{"v1.2.3", "v1.2.2", false}, // older
		{"v2.0.0", "v1.9.9", false}, // older, higher minor/patch
		{"dev", "v1.0.0", false},    // unparseable current - inconclusive
		{"v1.0.0", "not-a-version", false},
		{"v1.0.0", "v1.0.1-rc1", true}, // pre-release suffix stripped
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.latest); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func withFakeAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	original := apiBaseURL
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = original })
}

func TestLatestRelease(t *testing.T) {
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/someone/cloudup/releases/latest") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q", got)
		}
		json.NewEncoder(w).Encode(Release{TagName: "v1.2.3", HTMLURL: "https://github.com/someone/cloudup/releases/tag/v1.2.3"})
	})

	rel, err := LatestRelease(context.Background(), nil, "someone", "cloudup")
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want v1.2.3", rel.TagName)
	}
	if rel.HTMLURL != "https://github.com/someone/cloudup/releases/tag/v1.2.3" {
		t.Errorf("HTMLURL = %q", rel.HTMLURL)
	}
}

func TestLatestReleaseHTTPError(t *testing.T) {
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	if _, err := LatestRelease(context.Background(), nil, "someone", "cloudup"); err == nil {
		t.Fatal("expected an error for a 404 response")
	}
}
