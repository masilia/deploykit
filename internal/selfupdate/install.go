package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// maxBinary caps what is unpacked from an archive: the binary is about 12 MB.
var maxBinary int64 = 200 << 20

// InstallOptions say what to replace and for which platform.
type InstallOptions struct {
	// Exe is the file to replace. It must be the real file, not a link to it.
	Exe          string
	GOOS, GOARCH string
	// Check runs the unpacked binary before it takes the place of the old one; nil runs `<binary> version` and looks for the
	// version in its answer. Tests replace it.
	Check func(ctx context.Context, binary string, want Version) error
}

// Install downloads the archive of rel for the platform, checks it against the release's checksums.txt, unpacks the deploykit
// binary, makes sure it runs, and puts it in place of Exe. The old binary is kept next to it as <Exe>.old. Nothing is replaced
// unless every step before it succeeded; the returned path is the backup.
func (c *Client) Install(ctx context.Context, rel Release, opts InstallOptions) (string, error) {
	name := ArchiveName(rel.Version, opts.GOOS, opts.GOARCH)
	archiveURL, ok := rel.Assets[name]
	if !ok {
		return "", fmt.Errorf("release %s has no archive for %s/%s (%s)", rel.Version, opts.GOOS, opts.GOARCH, name)
	}
	sumsURL, ok := rel.Assets["checksums.txt"]
	if !ok {
		return "", fmt.Errorf("release %s has no checksums.txt: refusing to install what cannot be checked", rel.Version)
	}

	// Somewhere to write the new file beside the old one, so that the last step is a rename on the same disk. This is also the
	// check that the folder can be written, made before anything is downloaded.
	dir := filepath.Dir(opts.Exe)
	tmp, err := os.CreateTemp(dir, ".deploykit-update-*")
	if err != nil {
		return "", fmt.Errorf("cannot write in %s (%w): install deploykit in a folder you own, or run the update with the rights that folder needs", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // gone after the rename, and when anything failed
	defer func() { _ = tmp.Close() }()

	sums, err := c.get(ctx, sumsURL, maxChecksums, "")
	if err != nil {
		return "", fmt.Errorf("download checksums.txt: %w", err)
	}
	want, err := checksumOf(sums, name)
	if err != nil {
		return "", err
	}
	archive, err := c.get(ctx, archiveURL, maxArchive, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("checksum of %s does not match checksums.txt: the download is corrupt or was altered, nothing was installed", name)
	}

	if err := unpackBinary(archive, tmp); err != nil {
		return "", fmt.Errorf("unpack %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil { //nolint:gosec // an executable
		return "", err
	}
	check := opts.Check
	if check == nil {
		check = runVersion
	}
	if err := check(ctx, tmpPath, rel.Version); err != nil {
		return "", fmt.Errorf("the downloaded binary does not run as %s on this machine (%w): the current one is untouched", rel.Version, err)
	}
	return replace(opts.Exe, tmpPath)
}

// checksumOf finds the SHA-256 of a file in a checksums.txt ("<hex>  <name>" per line).
func checksumOf(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if len(f[0]) != 64 {
				return "", fmt.Errorf("checksums.txt has a malformed checksum for %s", name)
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt does not list %s", name)
}

// unpackBinary writes the file named exactly "deploykit" at the top of a tar.gz to w. Every other entry is ignored: a name with a
// folder in it, a link, a device. The archive also holds the dk link, which is not needed when the existing one points at deploykit.
func unpackBinary(archive []byte, w io.Writer) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("the archive holds no deploykit binary")
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || path.Clean(hdr.Name) != "deploykit" {
			continue
		}
		if hdr.Size > maxBinary {
			return fmt.Errorf("the binary is %d bytes, more than the %d allowed", hdr.Size, maxBinary)
		}
		n, err := io.Copy(w, io.LimitReader(tr, maxBinary+1))
		if err != nil {
			return err
		}
		if n == 0 || n > maxBinary {
			return fmt.Errorf("the binary has an unexpected size (%d bytes)", n)
		}
		return nil
	}
}

// runVersion runs `<binary> version` and looks for the expected version in what it prints.
func runVersion(ctx context.Context, binary string, want Version) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "version").CombinedOutput() //nolint:gosec // the file just verified against the release's checksums
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), want.String()) {
		return fmt.Errorf("it says %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// replace moves exe to <exe>.old and tmp to exe. If the second rename fails the first is undone.
func replace(exe, tmp string) (string, error) {
	backup := exe + ".old"
	if err := os.Rename(exe, backup); err != nil {
		return "", fmt.Errorf("keep the current binary as %s: %w", backup, err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		if rerr := os.Rename(backup, exe); rerr != nil {
			return "", fmt.Errorf("install the new binary: %w; and the old one could not be put back (it is %s): %w", err, backup, rerr)
		}
		return "", fmt.Errorf("install the new binary: %w (the current one is back in place)", err)
	}
	return backup, nil
}
