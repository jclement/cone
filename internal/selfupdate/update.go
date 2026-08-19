// `cone update`: fetch the release for this platform, verify it, and swap this binary for it.
//
// The order here is not decoration. Nothing is downloaded before we know we can write the
// target, nothing is unpacked before the signature and the checksum both pass, and the
// running binary is only ever replaced by an atomic rename — so an interrupted update leaves
// either the old cone or the new one, never half of either.

package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// Run is `cone update`. current is the version this binary was built with.
func Run(args []string, current string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	checkOnly := fs.Bool("check", false, "report whether an update exists; change nothing")
	yes := fs.Bool("yes", false, "install without asking")
	fs.Parse(args)

	if InContainer() {
		return errors.New("cone is running in a container — replacing the binary here is undone by " +
			"the next `docker run`. Pull a newer image instead")
	}
	// --check still works on a dev build (an unparseable version sorts oldest, so any
	// release reads as newer), but installing over one would throw away the local build.
	if current == "dev" && !*checkOnly {
		return errors.New("this is a dev build, not a release — `mise run build` is the update path here")
	}

	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()

	rel, err := Latest(ctx, current)
	if err != nil {
		return err
	}
	fmt.Printf("current:    %s\n", current)
	fmt.Printf("latest:     %s\n", rel.Tag)

	if !rel.NewerThan(current) {
		fmt.Println("\nalready up to date")
		return nil
	}
	if *checkOnly {
		fmt.Printf("\n%s is available — run: cone update\n%s\n", rel.Tag, rel.Page)
		return nil
	}
	if !*yes && !confirm(fmt.Sprintf("install %s over this binary?", rel.Tag)) {
		fmt.Println("cancelled")
		return nil
	}
	return Apply(ctx, rel, current)
}

// Apply downloads rel, verifies it, and replaces the running binary. It stops at the first
// step that fails and says which step that was; a failure never leaves a partly written
// binary behind. Progress goes to stdout in the same shape as `cone install`.
func Apply(ctx context.Context, rel *Release, current string) error {
	if rel.ChecksumsURL == "" || rel.BundleURL == "" {
		return fmt.Errorf("release %s ships no checksums.txt and sigstore bundle — refusing to "+
			"install an unverified download", rel.Tag)
	}
	target, err := targetPath()
	if err != nil {
		return err
	}
	// Homebrew owns the file it installed. Overwriting it in place would work and then be
	// silently reverted by the next brew command, so say the real answer instead.
	if strings.Contains(target, string(filepath.Separator)+"Cellar"+string(filepath.Separator)) {
		return fmt.Errorf("cone was installed by Homebrew (%s) — run `brew upgrade cone`", target)
	}
	if err := ensureWritable(target); err != nil {
		return err
	}

	client := &http.Client{Timeout: downloadTimeout}
	checksums, err := download(ctx, client, rel.ChecksumsURL, current)
	if err != nil {
		return fmt.Errorf("downloading checksums.txt: %w", err)
	}
	bundleJSON, err := download(ctx, client, rel.BundleURL, current)
	if err != nil {
		return fmt.Errorf("downloading checksums.txt.sigstore.json: %w", err)
	}
	if err := verifySignature(checksums, bundleJSON); err != nil {
		return fmt.Errorf("step 1 of 2, sigstore bundle over checksums.txt: %w", err)
	}
	fmt.Printf("signature:  ok — checksums.txt signed by the %s release workflow\n", repo)

	archive, err := download(ctx, client, rel.ArchiveURL, current)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", rel.Archive, err)
	}
	if err := installArchive(archive, checksums, rel.Archive, target); err != nil {
		return err
	}
	fmt.Printf("checksum:   ok — %s matches the signed checksums.txt\n", rel.Archive)
	fmt.Printf("installed:  %s is now %s\n", target, rel.Tag)
	return nil
}

// installArchive is the only path to disk, and it verifies before it writes: checksum,
// extract, replace, in that order. Keeping the comparison inside the function that installs
// means no future caller can accidentally install bytes nobody checked.
func installArchive(archive, checksums []byte, assetName, target string) error {
	want, err := checksumFor(checksums, assetName)
	if err != nil {
		return fmt.Errorf("step 2 of 2, archive checksum: %w", err)
	}
	sum := sha256.Sum256(archive)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("step 2 of 2, archive checksum: %s hashes to %s but the signed "+
			"checksums.txt says %s — refusing to install", assetName, got, want)
	}
	binary, err := extractBinary(archive, assetName)
	if err != nil {
		return err
	}
	return replaceBinary(binary, target)
}

// checksumFor pulls one asset's hex digest out of a checksums.txt (`<sha256>  <name>`).
func checksumFor(checksums []byte, assetName string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		if parts := strings.Fields(line); len(parts) == 2 && parts[1] == assetName {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", assetName)
}

// extractBinary reads the cone binary out of a release archive, tar.gz everywhere except
// Windows, where GoReleaser ships a zip.
func extractBinary(archive []byte, assetName string) ([]byte, error) {
	if strings.HasSuffix(assetName, ".zip") {
		return fromZip(archive)
	}
	return fromTarGz(archive)
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "cone.exe"
	}
	return "cone"
}

func fromTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("reading the archive: %w", err)
	}
	defer gz.Close()

	want := binaryName()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		if filepath.Base(hdr.Name) == want {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
	return nil, fmt.Errorf("no %s inside the archive", want)
}

func fromZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("reading the archive: %w", err)
	}
	want := binaryName()
	for _, f := range zr.File {
		if filepath.Base(f.Name) != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("reading %s from the archive: %w", f.Name, err)
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxDownload))
	}
	return nil, fmt.Errorf("no %s inside the archive", want)
}

// targetPath is the file to replace: the running binary with symlinks resolved, so that
// updating through ~/.local/bin/cone -> /somewhere/cone rewrites the real thing.
func targetPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// ensureWritable answers "can this installation be replaced at all?" before anything is
// downloaded, so a read-only install is a fast, clear no rather than a failure after ten
// megabytes. The probe is the same operation the install does: a temp file in the same
// directory.
func ensureWritable(target string) error {
	dir := filepath.Dir(target)
	probe, err := os.CreateTemp(dir, ".cone-update-probe-*")
	if err != nil {
		return cannotWrite(dir, err)
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}

// replaceBinary writes the new binary beside the old one and renames it over the top. Same
// directory means same filesystem, which makes the rename atomic — a reader either gets the
// whole old binary or the whole new one.
func replaceBinary(binary []byte, target string) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".cone-update-*")
	if err != nil {
		return cannotWrite(dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // a no-op once the rename below has moved it

	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("making %s executable: %w", tmpPath, err)
	}

	// UNIX lets a running binary be renamed over; Windows does not, so move the running
	// image aside first and put it back if the swap fails.
	aside := target + ".old"
	if runtime.GOOS == "windows" {
		os.Remove(aside)
		if err := os.Rename(target, aside); err != nil {
			return cannotWrite(target, err)
		}
	}
	if err := os.Rename(tmpPath, target); err != nil {
		if runtime.GOOS == "windows" {
			os.Rename(aside, target)
		}
		return cannotWrite(target, err)
	}
	if runtime.GOOS == "windows" {
		os.Remove(aside) // fails while the old image is still running; harmless either way
	}
	return nil
}

// cannotWrite turns a permission or read-only-filesystem error into something that says what
// to do about it, rather than an errno the user has to interpret.
func cannotWrite(path string, err error) error {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("cannot write to %s: %w — cone cannot update an installation it does "+
			"not own. Re-run as the user that installed it, or update it the way it was installed "+
			"(brew upgrade cone, mise run install, or a fresh download)", path, err)
	}
	return fmt.Errorf("replacing %s: %w", path, err)
}

// confirm asks on the terminal. Nothing on stdin — piped, redirected, or a closed session —
// is a no: an update that installs itself because nobody was there to object is exactly the
// surprise --yes exists to opt into.
func confirm(question string) bool {
	fmt.Printf("\n%s [y/N]: ", question)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Println()
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}
