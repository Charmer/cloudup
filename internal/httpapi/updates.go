package httpapi

import (
	"net/http"
	"strings"
	"time"

	"cloudup/internal/updatecheck"
)

type updateCheckResponse struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	ReleaseURL      string `json:"releaseUrl,omitempty"`
}

// handleUpdatesCheck is the only place cloudup ever talks to GitHub, and it
// only runs in direct response to this one request - there is no
// background timer or startup check anywhere in this codebase. Version and
// UpdateRepo are set once by cmd/server (see main.go's -update-repo flag
// and the version var set via -ldflags at build time); a "dev" build (no
// -ldflags) still reaches GitHub and reports the latest tag, it just can't
// claim an update is available since updatecheck.IsNewer can't compare
// "dev" against anything.
func (s *Server) handleUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxWithTimeout(r, 10*time.Second)
	defer cancel()

	owner, repo, ok := splitRepo(s.UpdateRepo)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server is not configured with a valid update repo (%q)", s.UpdateRepo)
		return
	}

	rel, err := updatecheck.LatestRelease(ctx, s.updateHTTPClient, owner, repo)
	if err != nil {
		writeError(w, http.StatusBadGateway, "checking for updates: %s", err)
		return
	}

	writeJSON(w, http.StatusOK, updateCheckResponse{
		CurrentVersion:  s.Version,
		LatestVersion:   rel.TagName,
		UpdateAvailable: updatecheck.IsNewer(s.Version, rel.TagName),
		ReleaseURL:      rel.HTMLURL,
	})
}

func splitRepo(ownerRepo string) (owner, repo string, ok bool) {
	owner, repo, found := strings.Cut(ownerRepo, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}
