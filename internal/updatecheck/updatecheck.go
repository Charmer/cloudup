// Package updatecheck looks up the latest published release of this
// project on GitHub and compares it against the running binary's version.
//
// This is deliberately request-scoped, not a background poller: cloudup is
// a local tool that otherwise never talks to the network except to the
// cloud storages the user configures, and staying that way (silent unless
// asked) matters more here than shaving a click off "am I up to date" -
// see internal/httpapi/updates.go, whose handler is the only caller and is
// itself only ever reached by a user clicking "Check for updates" in
// Settings.
package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// apiBaseURL is a package variable, not a constant, purely so tests can
// point it at an httptest.Server instead of real GitHub - see the
// googledrive/dropbox packages' oauthEndpoint for the same pattern.
var apiBaseURL = "https://api.github.com"

// Release is the subset of GitHub's release API response this package
// uses. See https://docs.github.com/en/rest/releases/releases#get-the-latest-release.
type Release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// LatestRelease fetches the latest published (non-draft, non-prerelease)
// GitHub release for owner/repo. No authentication is sent - this is a
// single, user-triggered call, nowhere near GitHub's 60/hour unauthenticated
// rate limit per IP.
func LatestRelease(ctx context.Context, httpClient *http.Client, owner, repo string) (Release, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/releases/latest", apiBaseURL, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("contacting GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub returned %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("decoding GitHub response: %w", err)
	}
	return rel, nil
}

// IsNewer reports whether latest is a newer version than current, comparing
// them as dotted major.minor.patch numbers (an optional leading "v" is
// stripped from both, and any trailing "-rc1"/"+build" suffix on the patch
// component is ignored). If either string doesn't parse as three numeric
// components - including the "dev" version a non-release build reports -
// the comparison is inconclusive and IsNewer returns false rather than
// guessing: the caller uses this to decide whether to tell the user an
// update exists, and a false positive is worse than staying silent.
func IsNewer(current, latest string) bool {
	c, ok1 := parseVersion(current)
	l, ok2 := parseVersion(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range 3 {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return out, false
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if idx := strings.IndexAny(p, "-+"); idx >= 0 {
			p = p[:idx]
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
