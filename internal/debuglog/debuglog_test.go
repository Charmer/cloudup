package debuglog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestEnabledReflectsEnvVar(t *testing.T) {
	t.Setenv(envVar, "")
	os.Unsetenv(envVar)
	if Enabled() {
		t.Fatal("Enabled() = true with CLOUDUP_DEBUG unset, want false")
	}

	t.Setenv(envVar, "1")
	if !Enabled() {
		t.Fatal("Enabled() = false with CLOUDUP_DEBUG=1, want true")
	}
}

func TestTransportPassesRequestsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	for _, enabled := range []string{"", "1"} {
		t.Setenv(envVar, enabled)

		client := &http.Client{Transport: Transport{}}
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	}
}
