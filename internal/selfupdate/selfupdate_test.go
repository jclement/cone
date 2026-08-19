package selfupdate

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		candidate string
		current   string
		want      bool
	}{
		{"0.2.0", "0.1.0", true},
		{"0.1.1", "0.1.0", true},
		{"1.0.0", "0.99.99", true},
		{"0.1.0", "0.1.0", false}, // equal is not newer
		{"0.1.0", "0.2.0", false}, // older
		{"0.9.9", "1.0.0", false},
		{"v0.2.0", "0.1.0", true},   // tag_name keeps its v
		{"v0.1.0", "v0.1.0", false}, // v on both sides
		{"0.2.0", "v0.1.0", true},   // mixed
		{"0.10.0", "0.9.0", true},   // numeric, not lexical
		{"0.2.0", "dev", true},      // an unreadable version sorts oldest
	}
	for _, c := range cases {
		t.Run(c.candidate+"_over_"+c.current, func(t *testing.T) {
			if got := isNewer(c.candidate, c.current); got != c.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
			}
		})
	}
}

func TestSkipReason(t *testing.T) {
	t.Setenv("CONE_NO_UPDATE_CHECK", "")
	if InContainer() {
		t.Skip("this machine is a container, where every check is skipped by design")
	}
	if got := skipReason("dev"); got == "" {
		t.Error("a dev build must skip the check")
	}
	if got := skipReason("0.1.0"); got != "" {
		t.Errorf("a released build should check; skipped because %q", got)
	}
	t.Setenv("CONE_NO_UPDATE_CHECK", "1")
	if got := skipReason("0.1.0"); got == "" {
		t.Error("CONE_NO_UPDATE_CHECK must skip the check")
	}
}

// TestStartSkipsDevBuild is the one that matters: a dev build must not merely ignore the
// answer, it must never ask. Local builds are what developers run in a loop, and a check
// against a version that can never match is pure noise on GitHub's rate limit.
func TestStartSkipsDevBuild(t *testing.T) {
	var calls atomic.Int64
	srv := releaseServer(t, "v0.2.0", &calls)
	defer srv.Close()

	if rel := Start("dev").Wait(2 * time.Second); rel != nil {
		t.Errorf("dev build reported an update: %+v", rel)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("dev build made %d github requests, want 0", n)
	}
}

func TestStartReportsNewerRelease(t *testing.T) {
	t.Setenv("CONE_NO_UPDATE_CHECK", "")
	if InContainer() {
		t.Skip("this machine is a container, where every check is skipped by design")
	}
	var calls atomic.Int64
	srv := releaseServer(t, "v0.2.0", &calls)
	defer srv.Close()

	rel := Start("0.1.0").Wait(5 * time.Second)
	if rel == nil {
		t.Fatal("no update reported for 0.1.0 against a 0.2.0 release")
	}
	if rel.Tag != "v0.2.0" {
		t.Errorf("tag = %q, want v0.2.0", rel.Tag)
	}
	if rel.ChecksumsURL == "" || rel.BundleURL == "" {
		t.Errorf("verification assets not resolved: %+v", rel)
	}
	if want := archiveName("0.2.0"); rel.Archive != want {
		t.Errorf("archive = %q, want %q", rel.Archive, want)
	}

	// The same release must not be offered to someone already running it.
	if rel := Start("0.2.0").Wait(5 * time.Second); rel != nil {
		t.Errorf("0.2.0 was offered an update to %s", rel.Tag)
	}
}

// releaseServer stands in for the GitHub releases API, counting requests so a test can prove
// one was never made. It points releasesURL at itself for the duration of the test.
func releaseServer(t *testing.T, tag string, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		base := "https://example.test/" + tag + "/"
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://example.test/releases/%s","assets":[
			{"name":%q,"browser_download_url":%q},
			{"name":"checksums.txt","browser_download_url":%q},
			{"name":"checksums.txt.sigstore.json","browser_download_url":%q}]}`,
			tag, tag,
			archiveName(tag[1:]), base+archiveName(tag[1:]),
			base+"checksums.txt", base+"checksums.txt.sigstore.json")
	}))
	previous := releasesURL
	releasesURL = srv.URL
	t.Cleanup(func() { releasesURL = previous })
	return srv
}

func TestArchiveNameMatchesGoReleaser(t *testing.T) {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	want := fmt.Sprintf("cone_1.2.3_%s_%s.%s", runtime.GOOS, runtime.GOARCH, ext)
	if got := archiveName("1.2.3"); got != want {
		t.Errorf("archiveName = %q, want %q", got, want)
	}
}

// Inside a container, `cone update` must refuse and say what to do instead — replacing the
// binary there is thrown away by the next `docker run`.
func TestUpdateRefusesInsideAContainer(t *testing.T) {
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	previous := containerMarkers
	containerMarkers = []string{marker}
	t.Cleanup(func() { containerMarkers = previous })

	var calls atomic.Int64
	srv := releaseServer(t, "v0.2.0", &calls)
	defer srv.Close()

	err := Run([]string{"--check"}, "0.1.0")
	if err == nil {
		t.Fatal("update ran inside a container")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("refusal does not say to pull a new image: %v", err)
	}
	if got := skipReason("0.1.0"); got == "" {
		t.Error("the background check must be skipped in a container too")
	}
}
