// Package selfupdate keeps an installed cone current: a background check that says a newer
// release exists, and `cone update`, which replaces this binary with that release.
//
// The verification chain is the whole point. A release is a set of archives, a checksums.txt
// listing their SHA-256s, and a keyless cosign bundle signed by the GitHub Actions workflow
// over that checksums.txt. `cone update` verifies the bundle, then the archive against the
// signed checksums, and installs nothing that fails either step. There is deliberately no
// --force and no --skip-verify: an escape hatch here would make self-update the cheapest way
// to attack every machine cone is installed on.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	repo = "jclement/cone"

	// The check is best-effort background work. 10s covers a cold DNS lookup on a slow
	// link and still guarantees the goroutine is gone long before anyone notices it.
	checkTimeout = 10 * time.Second

	// `cone update` pulls an archive over links we do not control.
	downloadTimeout = 5 * time.Minute

	// Nothing a release serves is anywhere near this. It bounds a hostile or broken
	// server that would otherwise stream until memory runs out.
	maxDownload = 128 << 20
)

// releasesURL is a var, not a const, so tests can point the check at an httptest server.
var releasesURL = "https://api.github.com/repos/" + repo + "/releases/latest"

// Release is the newest published release, already resolved to the assets this platform
// needs. Empty ChecksumsURL or BundleURL means the release cannot be verified, which Apply
// treats as a refusal rather than a reason to skip verification.
type Release struct {
	Version      string // "0.2.0" — no leading v, matching the archive names and ldflags
	Tag          string // "v0.2.0" — what a human recognises
	Page         string // the release page, for "what changed"
	Archive      string // asset filename for this GOOS/GOARCH
	ArchiveURL   string
	ChecksumsURL string
	BundleURL    string
}

// NewerThan reports whether this release is a newer semver than current.
func (r *Release) NewerThan(current string) bool { return isNewer(r.Version, current) }

// Check is an update check running in the background.
//
// It is started only by the commands that can show its answer — `cone version` and the TUI.
// cone is invoked in loops by agents, and a GitHub request per `cone ls` would burn the
// unauthenticated rate limit (60/hour) on a notice nobody would ever see.
type Check struct {
	result chan *Release
}

// Start kicks off a check for a release newer than current and returns immediately. The
// returned Check never blocks a command: if nothing has come back by the time the command
// finishes, the process exits and the answer is dropped.
//
// The check is skipped entirely for dev builds, when CONE_NO_UPDATE_CHECK is set, and inside
// a container — see skipReason. A skipped or failed check is silent; being told that we could
// not reach GitHub is noise on every single command.
func Start(current string) *Check {
	c := &Check{result: make(chan *Release, 1)}
	if skipReason(current) != "" {
		close(c.result)
		return c
	}
	go func() {
		defer close(c.result)
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		defer cancel()
		rel, err := Latest(ctx, current)
		if err != nil || !rel.NewerThan(current) {
			return
		}
		c.result <- rel
	}()
	return c
}

// Wait returns the newer release if the check finishes within grace, and nil otherwise —
// including when there is no update, the check failed, or it was never started.
func (c *Check) Wait(grace time.Duration) *Release {
	if c == nil {
		return nil
	}
	select {
	case rel := <-c.result:
		return rel
	case <-time.After(grace):
		return nil
	}
}

// Result blocks until the check finishes and returns the newer release, or nil if there is
// none. It is bounded by the check's own timeout, so it is safe from a goroutine that is not
// holding anything up — the TUI calls it from a tea.Cmd.
func (c *Check) Result() *Release {
	if c == nil {
		return nil
	}
	return <-c.result
}

// skipReason says why no update check should run, or "" when one should.
func skipReason(current string) string {
	switch {
	case current == "dev":
		return "dev build"
	case os.Getenv("CONE_NO_UPDATE_CHECK") != "":
		return "CONE_NO_UPDATE_CHECK is set"
	case InContainer():
		return "running in a container"
	}
	return ""
}

// containerMarkers are the files a container runtime leaves in the image root. It is a var
// so tests can point it somewhere writable — being inside a container is otherwise impossible
// to arrange from a test.
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"} // docker, then podman

// InContainer reports whether cone is running inside a container image, where replacing the
// binary is pointless: the next `docker run` restores the old one.
func InContainer() bool {
	for _, marker := range containerMarkers {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// ---- GitHub ----

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Latest fetches the newest published release and resolves the assets for this platform.
// It is an error for the release to carry no archive for this GOOS/GOARCH; a missing
// checksums or bundle asset is not, so that Apply can name that as the reason it refused.
func Latest(ctx context.Context, current string) (*Release, error) {
	client := &http.Client{Timeout: checkTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "cone/"+current)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking github for the latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api: %s", resp.Status)
	}

	var gh ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDownload)).Decode(&gh); err != nil {
		return nil, fmt.Errorf("reading the github release: %w", err)
	}

	rel := &Release{
		Version: strings.TrimPrefix(gh.TagName, "v"),
		Tag:     gh.TagName,
		Page:    gh.HTMLURL,
	}
	rel.Archive = archiveName(rel.Version)
	for _, a := range gh.Assets {
		switch a.Name {
		case rel.Archive:
			rel.ArchiveURL = a.URL
		case "checksums.txt":
			rel.ChecksumsURL = a.URL
		case "checksums.txt.sigstore.json":
			rel.BundleURL = a.URL
		}
	}
	if rel.ArchiveURL == "" {
		return nil, fmt.Errorf("release %s has no %s — nothing to install on %s/%s",
			gh.TagName, rel.Archive, runtime.GOOS, runtime.GOARCH)
	}
	return rel, nil
}

// archiveName is the GoReleaser archive name for this platform: it must match
// name_template in .goreleaser.yml, which uses the version without its v.
func archiveName(version string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("cone_%s_%s_%s.%s", version, runtime.GOOS, runtime.GOARCH, ext)
}

// download fetches url whole. Everything cone verifies is a few tens of megabytes at most,
// and holding the bytes in memory means a failed verification never leaves a file behind.
func download(ctx context.Context, client *http.Client, url, current string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cone/"+current)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload))
}

// ---- semver ----

// isNewer reports whether candidate is a newer semver than current. Both may carry a leading
// v, and anything after the patch number (a -rc1 or +build suffix) is ignored: GitHub's
// "latest" endpoint never returns a prerelease, so the three numbers decide it.
func isNewer(candidate, current string) bool {
	c, r := parseSemver(candidate), parseSemver(current)
	for i := range c {
		if c[i] != r[i] {
			return c[i] > r[i]
		}
	}
	return false
}

// parseSemver pulls major/minor/patch out of a version string. Anything it cannot read is a
// zero, which makes an unparseable version the oldest possible one rather than an error —
// there is no useful way to fail a background check on a malformed tag.
func parseSemver(s string) [3]int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	var parts [3]int
	for i, field := range strings.SplitN(s, ".", 3) {
		n, err := strconv.Atoi(field)
		if err != nil {
			break
		}
		parts[i] = n
	}
	return parts
}
