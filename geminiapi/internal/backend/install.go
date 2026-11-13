package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// downloadStallTimeout abandons a download that has produced no bytes for this
// long. It is a per-read stall budget, not a total budget: the asset is tens of
// megabytes and a slow-but-moving link must be allowed to finish.
var downloadStallTimeout = 60 * time.Second

// Downloader fetches one asset. Split into a field so tests can swap it without
// touching the filesystem or the network.
type Downloader func(dest string, url string, proxy string) error

// HTTPDownload is the production Downloader: it streams the asset to dest
// through an optional proxy, aborting when no bytes arrive for
// downloadStallTimeout.
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

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", "cpa-plugin-geminiapi")
	// A redirect to a different host is how a release asset is served, so
	// redirects are allowed; the digest check is what makes that safe.
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d from %s", resp.StatusCode, rawURL)
	}

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create download target: %w", err)
	}
	defer func() { _ = out.Close() }()

	stall := time.NewTimer(downloadStallTimeout)
	defer stall.Stop()
	type readResult struct {
		n   int
		err error
	}
	for {
		buf := make([]byte, 64*1024)
		ch := make(chan readResult, 1)
		go func() {
			n, rerr := resp.Body.Read(buf)
			ch <- readResult{n: n, err: rerr}
		}()
		select {
		case <-stall.C:
			return fmt.Errorf("download failed: no data for %s", downloadStallTimeout)
		case res := <-ch:
			if res.n > 0 {
				if _, werr := out.Write(buf[:res.n]); werr != nil {
					return fmt.Errorf("write download target: %w", werr)
				}
				stall.Reset(downloadStallTimeout)
			}
			if res.err == io.EOF {
				return nil
			}
			if res.err != nil {
				return fmt.Errorf("read download stream: %w", res.err)
			}
		}
	}
}

// errDigestMismatch marks a download whose bytes are not the pinned ones. It is
// distinct so callers can report "refused to replace / refused to start"
// instead of a generic failure.
var errDigestMismatch = errors.New("sha256 mismatch")

// installedPath is where a verified binary lives for one data directory.
func installedPath(dataDir string) string {
	name := BinaryName
	if isWindows() {
		name += ".exe"
	}
	return filepath.Join(dataDir, "bin", name)
}

// Install ensures a verified binary exists in dataDir and returns its path.
//
// A binary already present with the pinned digest is reused without touching
// the network. Anything else — missing, unreadable, or a digest that is not the
// pin — is replaced ONLY after the replacement's digest also matches. A
// mismatch never leaves a partially trusted file at the launch path: the
// download goes to a temporary name and is renamed into place only after it
// verifies.
func Install(dataDir string, pin Pin, downloadURL, proxy string, download Downloader) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data directory is not configured")
	}
	binDir := filepath.Join(dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", binDir, err)
	}
	binPath := installedPath(dataDir)

	if digest, err := FileSHA256(binPath); err == nil && digest == pin.SHA256 {
		return binPath, nil
	}

	if download == nil {
		download = HTTPDownload
	}
	tmp := filepath.Join(binDir, fmt.Sprintf("%s.download-%d", BinaryName, time.Now().UnixNano()))
	defer func() { _ = os.Remove(tmp) }()

	if err := download(tmp, downloadURL, proxy); err != nil {
		return "", err
	}
	digest, err := FileSHA256(tmp)
	if err != nil {
		return "", fmt.Errorf("hash downloaded binary: %w", err)
	}
	if digest != pin.SHA256 {
		// The temporary file is removed by the deferred cleanup; the launch
		// path is untouched, so a previously working install stays working.
		return "", fmt.Errorf("%w: %s %s expected %s, downloaded bytes hash to %s (release %s)",
			errDigestMismatch, BinaryName, ReleaseVersion, pin.SHA256, digest, ReleasePageURL())
	}
	if err := os.Chmod(tmp, 0o700); err != nil && !isWindows() {
		return "", fmt.Errorf("make binary executable: %w", err)
	}
	if err := os.Rename(tmp, binPath); err != nil {
		return "", fmt.Errorf("install verified binary: %w", err)
	}
	return binPath, nil
}

// VerifiedPath reports whether a verified binary already exists. It returns the
// path and true only when the file's digest equals the pin.
func VerifiedPath(dataDir string, pin Pin) (string, bool) {
	binPath := installedPath(dataDir)
	digest, err := FileSHA256(binPath)
	if err != nil || digest != pin.SHA256 {
		return binPath, false
	}
	return binPath, true
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
