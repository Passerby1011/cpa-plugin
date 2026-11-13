package backend

// Installing a pinned backend binary.
//
// # Two asset shapes
//
// gemini-web2api-go publishes a RAW binary per platform: the release digest
// covers the bytes that will be executed. AIStudio2API publishes an ARCHIVE
// (.tar.gz / .zip) holding a package directory: the release digest covers the
// archive, and the release manifest separately publishes the executable's own
// digest. Both are verified, in this order:
//
//	1. asset bytes vs the pin (before anything is unpacked)
//	2. extracted executable vs BinarySHA256 (after unpacking)
//	3. extracted executable vs BinarySHA256 again on every launch (VerifiedPath)
//
// A mismatch at any step leaves the previous verified install untouched: the
// download goes to a temporary name and the unpack goes to a staging directory,
// so only a fully verified result reaches the launch path.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Downloader fetches one asset. Split into a field so tests can swap it without
// touching the filesystem or the network.
type Downloader func(dest string, url string, proxy string) error

// downloadStallTimeout abandons a download that has produced no bytes for this
// long. It is a per-read stall budget, not a total budget: the asset is tens of
// megabytes and a slow-but-moving link must be allowed to finish.
var downloadStallTimeout = 60 * time.Second

// HTTPDownload is the production Downloader: it streams the asset to dest
// through an optional proxy, aborting when no bytes arrive for
// downloadStallTimeout, and RESUMING after an interrupted transfer.
//
// # Why resume matters here
//
// The pinned assets are tens of megabytes served from a release CDN. A flaky
// link drops mid-transfer ("unexpected EOF"), and restarting from zero after a
// partial transfer either never finishes or wastes the bandwidth already spent.
// The digest check is what makes resume safe: every attempt appends to the same
// temporary file and the final sha256 covers the whole of it, so a corrupted
// middle byte still fails the gate.
func HTTPDownload(dest, rawURL, proxy string) error {
	transport := &http.Transport{}
	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return fmt.Errorf("invalid proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	} else {
		transport.Proxy = nil // never inherit HTTP_PROXY for a pinned download
	}
	client := &http.Client{Transport: transport, Timeout: 0}

	// A short read that ends in EOF is a dropped connection, not a finished
	// asset: the total length is known from Content-Length, so compare.
	var total int64
	for attempt := 0; attempt <= downloadResumeAttempts; attempt++ {
		var offset int64
		if info, statErr := os.Stat(dest); statErr == nil {
			offset = info.Size()
		}
		if total > 0 && offset == total {
			return nil // already complete
		}

		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return fmt.Errorf("build download request: %w", err)
		}
		req.Header.Set("User-Agent", "cpa-plugin-geminiapi")
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		resp, err := client.Do(req)
		if err != nil {
			if attempt == downloadResumeAttempts {
				return fmt.Errorf("download failed: %w", err)
			}
			time.Sleep(downloadRetryBackoff)
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
			// A server that ignores Range restarts the body; the file must be
			// truncated so the digest covers the real asset only.
			if offset > 0 {
				_ = resp.Body.Close()
				if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("reset partial download: %w", err)
				}
				offset = 0
				continue
			}
			total = resp.ContentLength
		case http.StatusPartialContent:
			if resp.ContentLength >= 0 && total == 0 {
				total = offset + resp.ContentLength
			}
		default:
			_ = resp.Body.Close()
			return fmt.Errorf("download failed: HTTP %d from %s", resp.StatusCode, rawURL)
		}

		written, writeErr := appendTo(dest, resp.Body)
		_ = resp.Body.Close()
		if writeErr != nil {
			return writeErr
		}
		offset += written
		if total > 0 && offset >= total {
			return nil
		}
		if total == 0 && written == 0 {
			// No length and no bytes: the server answered with an empty body.
			return nil
		}
		if attempt == downloadResumeAttempts {
			if total > 0 {
				return fmt.Errorf("download failed: incomplete after %d attempts (%d of %d bytes)", attempt+1, offset, total)
			}
			return nil
		}
		// Partial transfer: resume from the current offset.
		time.Sleep(downloadRetryBackoff)
	}
	return fmt.Errorf("download failed: attempts exhausted")
}

// downloadResumeAttempts bounds how many times a dropped transfer is resumed.
const downloadResumeAttempts = 12

// downloadRetryBackoff is the pause between resume attempts.
var downloadRetryBackoff = 1500 * time.Millisecond

// appendTo copies body into dest, appending, with a stall budget per read.
func appendTo(dest string, body io.Reader) (int64, error) {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create download target: %w", err)
	}
	defer func() { _ = out.Close() }()

	var total int64
	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, 64*1024)
	for {
		stall := time.NewTimer(downloadStallTimeout)
		ch := make(chan readResult, 1)
		go func() {
			n, rerr := body.Read(buf)
			ch <- readResult{n: n, err: rerr}
		}()
		select {
		case <-stall.C:
			stall.Stop()
			return total, fmt.Errorf("download failed: no data for %s", downloadStallTimeout)
		case res := <-ch:
			stall.Stop()
			if res.n > 0 {
				if _, werr := out.Write(buf[:res.n]); werr != nil {
					return total, fmt.Errorf("write download target: %w", werr)
				}
				total += int64(res.n)
			}
			if res.err == io.EOF {
				return total, nil
			}
			if res.err != nil {
				// Report the bytes already written so the caller can resume.
				return total, nil
			}
		}
	}
}

// errDigestMismatch marks an asset whose bytes are not the pinned ones. It is
// distinct so callers can report "refused to replace / refused to start"
// instead of a generic failure.
var errDigestMismatch = errors.New("sha256 mismatch")

// errBinaryDigestMismatch marks an UNPACKED executable whose bytes are not the
// ones the release manifest describes. It is distinct from errDigestMismatch
// because the operator-facing meaning differs: the archive was right, its
// contents were not.
var errBinaryDigestMismatch = errors.New("packaged binary sha256 mismatch")

// errUnsafeArchiveEntry marks an archive carrying a path that could escape the
// staging directory (absolute path, "..", drive letter, symlink or special
// file).
var errUnsafeArchiveEntry = errors.New("unsafe archive entry")

// errBinaryMissing marks an archive that does not contain the executable it is
// supposed to.
var errBinaryMissing = errors.New("archive does not contain the expected executable")

// Install ensures a verified binary exists and returns its path.
//
// A binary already present with the expected digest is reused without touching
// the network. Anything else — missing, unreadable, or a digest that is not the
// pin — is replaced ONLY after the replacement also verifies.
func (f Flavor) Install(dataDir, downloadURL, proxy string, download Downloader) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", fmt.Errorf("data directory is not configured")
	}
	layout := f.Layout(dataDir)
	if err := os.MkdirAll(layout.BinaryDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", layout.BinaryDir, err)
	}
	if path, ok := f.VerifiedPath(dataDir); ok {
		return path, nil
	}

	pin, err := f.CurrentPin()
	if err != nil {
		return "", err
	}
	if download == nil {
		download = HTTPDownload
	}
	staged := filepath.Join(layout.BinaryDir, fmt.Sprintf("%s.download-%d", f.BinaryName, time.Now().UnixNano()))
	defer func() { _ = os.Remove(staged) }()
	if err := download(staged, downloadURL, proxy); err != nil {
		return "", err
	}
	return f.installVerified(dataDir, pin, staged)
}

// installVerified checks a downloaded asset and installs it.
func (f Flavor) installVerified(dataDir string, pin Pin, staged string) (string, error) {
	layout := f.Layout(dataDir)
	digest, err := FileSHA256(staged)
	if err != nil {
		return "", fmt.Errorf("hash downloaded asset: %w", err)
	}
	if digest != pin.SHA256 {
		// The staged file is removed by the caller's deferred cleanup; the
		// launch path is untouched, so a previously working install survives.
		return "", fmt.Errorf("%w: %s %s expected %s, downloaded bytes hash to %s (release %s)",
			errDigestMismatch, f.Label(), f.ReleaseVersion, pin.SHA256, digest, f.ReleasePageURL())
	}
	if !f.Archive {
		if err := os.Chmod(staged, 0o700); err != nil && !isWindows() {
			return "", fmt.Errorf("make binary executable: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(layout.Binary), 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", filepath.Dir(layout.Binary), err)
		}
		if err := os.Rename(staged, layout.Binary); err != nil {
			return "", fmt.Errorf("install verified binary: %w", err)
		}
		return layout.Binary, nil
	}
	return f.installArchive(dataDir, pin, staged)
}

// installArchive unpacks a verified archive and installs its package.
func (f Flavor) installArchive(dataDir string, pin Pin, staged string) (string, error) {
	layout := f.Layout(dataDir)
	staging, err := os.MkdirTemp(dataDir, ".unpack-")
	if err != nil {
		return "", fmt.Errorf("create unpack staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := UnpackArchive(staged, staging); err != nil {
		return "", err
	}
	pkgDir, binary, err := FindPackage(staging, filepath.Base(layout.Binary))
	if err != nil {
		return "", err
	}
	expected, err := f.ExpectedBinaryDigest(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	binaryDigest, err := FileSHA256(binary)
	if err != nil {
		return "", fmt.Errorf("hash unpacked executable: %w", err)
	}
	if binaryDigest != expected {
		return "", fmt.Errorf("%w: %s %s expected %s, unpacked executable hashes to %s (release %s)",
			errBinaryDigestMismatch, f.Label(), f.ReleaseVersion, expected, binaryDigest, f.ReleasePageURL())
	}
	if err := os.Chmod(binary, 0o700); err != nil && !isWindows() {
		return "", fmt.Errorf("make binary executable: %w", err)
	}
	if err := swapDirectory(layout.BinaryDir, pkgDir); err != nil {
		return "", err
	}
	return layout.Binary, nil
}

// swapDirectory moves src into dst, replacing dst. The previous contents are
// removed only after src is in place, so a failed swap leaves the old package
// usable — a verified archive alone does not justify an unrecoverable install.
func swapDirectory(dst, src string) error {
	fresh := dst + fmt.Sprintf(".new-%d", time.Now().UnixNano())
	if err := os.Rename(src, fresh); err != nil {
		return fmt.Errorf("stage unpacked package: %w", err)
	}
	backup := ""
	if _, err := os.Stat(dst); err == nil {
		backup = dst + fmt.Sprintf(".old-%d", time.Now().UnixNano())
		if err := os.Rename(dst, backup); err != nil {
			_ = os.RemoveAll(fresh)
			return fmt.Errorf("move previous package aside: %w", err)
		}
	}
	if err := os.Rename(fresh, dst); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dst)
		}
		_ = os.RemoveAll(fresh)
		return fmt.Errorf("install unpacked package: %w", err)
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	_ = os.Chmod(dst, 0o700)
	return nil
}

// UnpackArchive extracts a .tar.gz or .zip into dest, refusing any entry that
// could escape it.
//
// The refusal list is what makes unpacking a downloaded archive safe: an
// absolute path, a ".." component, a Windows drive letter or a symlink could
// each write outside dest, and a symlink could make a later lookup resolve
// somewhere entirely different.
func UnpackArchive(archivePath, dest string) error {
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return unpackZip(archivePath, dest)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return unpackTarGz(archivePath, dest)
	default:
		return fmt.Errorf("unsupported archive type %q: only .zip and .tar.gz are pinned", filepath.Base(archivePath))
	}
}

func unpackTarGz(archivePath, dest string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read gzip header: %w", err)
	}
	defer func() { _ = gz.Close() }()

	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive entry: %w", err)
		}
		target, err := safeArchivePath(dest, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("create archive directory: %w", err)
			}
		case tar.TypeReg:
			if err := writeArchiveFile(target, reader, os.FileMode(header.Mode)); err != nil {
				return err
			}
		default:
			// Symlinks, hard links and special files are refused outright:
			// none belongs in a release archive, and each can move a later
			// write outside the staging directory.
			return fmt.Errorf("%w: %s (type %q)", errUnsafeArchiveEntry, header.Name, string(header.Typeflag))
		}
	}
}

func unpackZip(archivePath, dest string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = reader.Close() }()
	for _, entry := range reader.File {
		target, err := safeArchivePath(dest, entry.Name)
		if err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("create archive directory: %w", err)
			}
			continue
		}
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 || !mode.IsRegular() {
			return fmt.Errorf("%w: %s (mode %s)", errUnsafeArchiveEntry, entry.Name, mode)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create archive directory: %w", err)
		}
		opened, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open zip entry: %w", err)
		}
		out, createErr := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, archiveFilePerm(mode))
		if createErr != nil {
			_ = opened.Close()
			return fmt.Errorf("create archive file: %w", createErr)
		}
		_, copyErr := io.Copy(out, opened)
		_ = out.Close()
		_ = opened.Close()
		if copyErr != nil {
			return fmt.Errorf("write archive file: %w", copyErr)
		}
	}
	return nil
}

// archiveFilePerm keeps an executable bit when the archive has one and never
// grants group/other write.
func archiveFilePerm(mode os.FileMode) os.FileMode {
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o600
	}
	return perm | 0o600
}

// writeArchiveFile creates target (and its parents) and copies contents into it
// with the archive's permissions, never granting group/other write.
func writeArchiveFile(target string, contents io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, archiveFilePerm(mode))
	if err != nil {
		return fmt.Errorf("create archive file: %w", err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, contents); err != nil {
		return fmt.Errorf("write archive file: %w", err)
	}
	return nil
}

// safeArchivePath joins an archive entry name onto dest, refusing anything that
// resolves outside it.
func safeArchivePath(dest, name string) (string, error) {
	normalized := strings.ReplaceAll(name, "\\", "/")
	if normalized == "" {
		return "", fmt.Errorf("%w: empty entry name", errUnsafeArchiveEntry)
	}
	if strings.HasPrefix(normalized, "/") || normalized == ".." ||
		strings.HasPrefix(normalized, "../") || strings.Contains(normalized, "/../") {
		return "", fmt.Errorf("%w: %s escapes the install directory", errUnsafeArchiveEntry, name)
	}
	if len(normalized) >= 2 && normalized[1] == ':' {
		return "", fmt.Errorf("%w: %s names a drive", errUnsafeArchiveEntry, name)
	}
	target := filepath.Join(dest, filepath.FromSlash(normalized))
	// Belt and braces: the joined path must still be under dest.
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s escapes the install directory", errUnsafeArchiveEntry, name)
	}
	return target, nil
}

// FindPackage locates the executable inside an unpacked archive and returns the
// directory that must become the package.
//
// AI Studio's archive wraps everything in a single top-level directory and
// carries sibling files the service needs (runtime assets, licence files), so
// the whole top-level directory is the package. Shifting that directory — rather
// than copying one file out of it — is what keeps a self-contained package
// self-contained.
func FindPackage(staging, binaryName string) (dir string, binary string, err error) {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return "", "", fmt.Errorf("read unpacked archive: %w", err)
	}
	root := staging
	if len(entries) == 1 && entries[0].IsDir() {
		root = filepath.Join(staging, entries[0].Name())
	}
	if info, statErr := os.Stat(filepath.Join(root, binaryName)); statErr == nil && info.Mode().IsRegular() {
		return root, filepath.Join(root, binaryName), nil
	}
	// Deeper layouts exist; take the first regular file with the exact name.
	var found string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || found != "" {
			return nil
		}
		if !entry.IsDir() && entry.Name() == binaryName {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", "", fmt.Errorf("%w: %s (release archive layout changed?)", errBinaryMissing, binaryName)
	}
	return filepath.Dir(found), found, nil
}

// VerifiedPath reports whether a verified binary already exists. It returns the
// path and true only when the file's digest equals what the pin requires.
func (f Flavor) VerifiedPath(dataDir string) (string, bool) {
	layout := f.Layout(dataDir)
	expected, err := f.ExpectedBinaryDigest(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return layout.Binary, false
	}
	digest, err := FileSHA256(layout.Binary)
	if err != nil || digest != expected {
		return layout.Binary, false
	}
	return layout.Binary, true
}

// FileSHA256 is the lowercase hex digest of a file's bytes.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RedactHashes shortens a digest for logs and error text. A digest is not a
// secret, but a full 64-char hex string in a status line is unreadable noise.
func RedactHashes(digest string) string {
	digest = strings.TrimSpace(digest)
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12] + "…"
}

// --- gemini-web shorthands ---
//
// They keep the browser backend's supervisor and tests working unchanged now
// that the two backends share one installer.

// installedPath is where gemini-web2api-go's verified binary lives.
func installedPath(dataDir string) string {
	return geminiWebFlavor.Layout(dataDir).Binary
}

// Install ensures gemini-web2api-go's verified binary exists in dataDir.
//
// A caller-supplied pin (a test's fixture) still governs the check, so the
// digest the caller asserts is the digest that matters.
func Install(dataDir string, pin Pin, downloadURL, proxy string, download Downloader) (string, error) {
	f := geminiWebFlavor
	if pin.SHA256 != "" {
		f.Pins = map[string]Pin{PlatformKey(runtime.GOOS, runtime.GOARCH): pin}
	}
	return f.Install(dataDir, downloadURL, proxy, download)
}

// VerifiedPath reports whether gemini-web2api-go's verified binary exists.
func VerifiedPath(dataDir string, pin Pin) (string, bool) {
	path := installedPath(dataDir)
	digest, err := FileSHA256(path)
	if err != nil || digest != pin.SHA256 {
		return path, false
	}
	return path, true
}
