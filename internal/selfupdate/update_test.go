package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const assetName = "cone_0.2.0_test.tar.gz"

// TestInstallArchiveRejectsChecksumMismatch is the guarantee the whole package exists for:
// bytes that do not match the signed checksums.txt never reach the binary on disk.
func TestInstallArchiveRejectsChecksumMismatch(t *testing.T) {
	target := existingBinary(t)
	archive := tarGzWithBinary(t, []byte("the new cone"))
	wrong := "0000000000000000000000000000000000000000000000000000000000000000 " + assetName

	err := installArchive(archive, []byte(wrong), assetName, target)
	if err == nil {
		t.Fatal("a mismatched checksum was installed")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error does not name the failing step: %v", err)
	}
	if got := read(t, target); got != "the old cone" {
		t.Errorf("target was modified despite the failure: %q", got)
	}
}

// A checksums.txt that says nothing about our asset is just as unverifiable as a wrong one.
func TestInstallArchiveRejectsMissingChecksumEntry(t *testing.T) {
	target := existingBinary(t)
	archive := tarGzWithBinary(t, []byte("the new cone"))
	other := checksumLine(t, []byte("something else"), "cone_0.2.0_other.tar.gz")

	err := installArchive(archive, []byte(other), assetName, target)
	if err == nil {
		t.Fatal("an asset with no checksum entry was installed")
	}
	if got := read(t, target); got != "the old cone" {
		t.Errorf("target was modified despite the failure: %q", got)
	}
}

func TestInstallArchiveReplacesBinary(t *testing.T) {
	target := existingBinary(t)
	archive := tarGzWithBinary(t, []byte("the new cone"))

	if err := installArchive(archive, []byte(checksumLine(t, archive, assetName)), assetName, target); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := read(t, target); got != "the new cone" {
		t.Errorf("target = %q, want the new binary", got)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: %v", info.Mode())
	}
	// The temp file the rename came from must not be left lying around next to the binary.
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cone-update-") {
			t.Errorf("left behind %s", e.Name())
		}
	}
}

// A read-only install — an old Homebrew Cellar, a root-owned /usr/local/bin — has to produce
// an instruction, not a panic or an errno.
func TestEnsureWritableExplainsAReadOnlyInstall(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory, so there is nothing to detect")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, binaryName())
	if err := os.WriteFile(target, []byte("the old cone"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	err := ensureWritable(target)
	if err == nil {
		t.Fatal("a read-only install looked writable")
	}
	if !strings.Contains(err.Error(), "cannot write to") {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}

func TestChecksumFor(t *testing.T) {
	checksums := []byte("aaa  cone_0.2.0_darwin_arm64.tar.gz\nbbb  cone_0.2.0_linux_amd64.tar.gz\n")
	got, err := checksumFor(checksums, "cone_0.2.0_linux_amd64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got != "bbb" {
		t.Errorf("checksum = %q, want bbb", got)
	}
	if _, err := checksumFor(checksums, "cone_0.2.0_windows_arm64.zip"); err == nil {
		t.Error("expected an error for an asset that is not listed")
	}
}

// ---- helpers ----

// existingBinary is a stand-in for the installed cone: a file the install path must either
// replace wholesale or leave exactly as it found it.
func existingBinary(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), binaryName())
	if err := os.WriteFile(target, []byte("the old cone"), 0o755); err != nil {
		t.Fatal(err)
	}
	return target
}

func tarGzWithBinary(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"README.md", []byte("# cone")}, // the real archives carry more than the binary
		{binaryName(), binary},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func checksumLine(t *testing.T, data []byte, name string) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + "  " + name + "\n"
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
