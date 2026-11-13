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
// downloadStallTimeout, and RESUMING after an interrupted transfer.
//
// # Why resume matters here
//
// The pinned asset is ~18 MB served from a release CDN. A flaky link drops
// mid-transfer ("unexpected EOF"), and restarting from zero after a partial
// transfer either never finishes or wastes the bandwidth already spent. The
// digest check is what makes resume safe: every attempt appends to the same
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
