package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFixture writes a file with known bytes and returns its digest.
func writeFixture(t *testing.T, path string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, content, 0o700); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func TestPinForCurrentPlatformIsKnownAndComplete(t *testing.T) {
	pin, err := CurrentPin()
	if err != nil {
		// An unsupported platform must be a clear, actionable error rather
		// than a silent guess.
		if !strings.Contains(err.Error(), "no pinned") {
			t.Fatalf("unexpected error text: %v", err)
		}
		t.Skipf("platform %s/%s is not pinned: %v", runtime.GOOS, runtime.GOARCH, err)
	}
	if len(pin.SHA256) != 64 {
		t.Fatalf("pin sha256 must be 64 hex chars, got %d (%q)", len(pin.SHA256), pin.SHA256)
	}
	if _, hexErr := hex.DecodeString(pin.SHA256); hexErr != nil {
		t.Fatalf("pin sha256 is not hex: %v", hexErr)
	}
	if pin.Asset == "" || !strings.HasSuffix(pin.Asset, ".exe") && runtime.GOOS == "windows" {
		t.Fatalf("windows pin must name an .exe asset, got %q", pin.Asset)
	}
	if !strings.Contains(pin.Asset, ReleaseVersion) {
		t.Fatalf("asset %q does not carry version %q", pin.Asset, ReleaseVersion)
	}
}

// TestPinsCoverEveryDocumentedPlatform asserts the table is not silently
// truncated: a missing entry means that platform cannot run at all.
func TestPinsCoverEveryDocumentedPlatform(t *testing.T) {
	for _, key := range []string{
		"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64",
	} {
		pin, ok := Pins[key]
		if !ok {
			t.Fatalf("missing pin for %s", key)
		}
		if len(pin.SHA256) != 64 {
			t.Fatalf("%s digest is %d chars", key, len(pin.SHA256))
		}
	}
}

// TestPinRejectsUnknownPlatform is the negative control for PinFor.
func TestPinRejectsUnknownPlatform(t *testing.T) {
	if _, err := PinFor("plan9", "386"); err == nil {
		t.Fatal("PinFor accepted an unsupported platform; it must refuse rather than guess")
	}
}

func TestPinMatchesMagpieManifest(t *testing.T) {
	// These digests are copied verbatim from
	// magpie-gemini-web lib/backend-release.json. If they drift, the plugin
	// downloads a binary the upstream manifest does not describe.
	manifest := map[string]string{
		"linux/amd64":   "ec3ee723dbd6f99b6e874eaf2aab23b7c8afb3ab5c2a3498b51fea27b1f42ded",
		"linux/arm64":   "bcadd1a679c36ca1eb582835c4a9d01a157612951cd9a2f58c154aa20fc2e893",
		"darwin/amd64":  "7f264075c556592f9753a43358c4cf1bd90b3d41343d88836f835d20d9a70f21",
		"darwin/arm64":  "9c495eb5f6e97fceb9a97fe21b4d33f1127592aa96a8206e3a243244571d9c37",
		"windows/amd64": "8cd8f92133873fe4e15fed6081404ad609c90e8cc7701154d008e23c8b133648",
		"windows/arm64": "f4d7d46be7fbfe12e6ae26ef3dbd861519611e5dbd364e335ab7a0facecf87ce",
	}
	for key, want := range manifest {
		if got := Pins[key].SHA256; got != want {
			t.Fatalf("%s pin = %s, upstream manifest has %s", key, got, want)
		}
	}
}

func TestInstallReusesVerifiedBinaryWithoutDownloading(t *testing.T) {
	dataDir := t.TempDir()
	pin := Pin{Asset: "fixture.bin", SHA256: "0"}
	content := []byte("verified backend bytes")
	wanted := writeFixture(t, installedPath(dataDir), content)
	pin.SHA256 = wanted

	called := false
	path, err := Install(dataDir, pin, "https://example.invalid/x", "", func(dst, url, proxy string) error {
		called = true
		return errors.New("must not be called")
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if called {
		t.Fatal("Install downloaded despite a verified binary being present")
	}
	if path != installedPath(dataDir) {
		t.Fatalf("install path = %q, want %q", path, installedPath(dataDir))
	}
}

// TestInstallRefusesDigestMismatch is the acceptance criterion "sha256 不匹配拒启动".
// A mismatching download must leave the launch path untouched and return a
// distinct, readable error.
func TestInstallRefusesDigestMismatch(t *testing.T) {
	dataDir := t.TempDir()
	pin := Pin{Asset: "fixture.bin", SHA256: strings.Repeat("a", 64)}

	path, err := Install(dataDir, pin, "https://example.invalid/x", "", func(dst, url, proxy string) error {
		// Write bytes that are NOT the pinned ones, as a corrupted or
		// substituted download would.
		return os.WriteFile(dst, []byte("malicious replacement"), 0o700)
	})
	if err == nil {
		t.Fatal("Install accepted a download whose digest does not match the pin")
	}
	if !errors.Is(err, errDigestMismatch) {
		t.Fatalf("error is not classified as a digest mismatch: %v", err)
	}
	if !strings.Contains(err.Error(), "expected") || !strings.Contains(err.Error(), "hash to") {
		t.Fatalf("error text does not name both digests: %v", err)
	}
	if path != "" {
		t.Fatalf("Install returned a path for a rejected binary: %q", path)
	}
	// The launch path must not exist: nothing unverified is ever placed there.
	if _, statErr := os.Stat(installedPath(dataDir)); statErr == nil {
		t.Fatal("a rejected binary was written to the launch path")
	}
	// And no partial download is left behind.
	entries, _ := os.ReadDir(filepath.Join(dataDir, "bin"))
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".download-") {
			t.Fatalf("temporary download file was left behind: %s", entry.Name())
		}
	}
}

// TestInstallKeepsWorkingBinaryWhenReplacementFails asserts that a failed
// replacement never destroys a previously verified install.
func TestInstallKeepsWorkingBinaryWhenReplacementFails(t *testing.T) {
	dataDir := t.TempDir()
	goodBytes := []byte("good backend")
	goodDigest := writeFixture(t, installedPath(dataDir), goodBytes)
	goodPin := Pin{Asset: "fixture.bin", SHA256: goodDigest}

	if _, err := Install(dataDir, goodPin, "u", "", nil); err != nil {
		t.Fatalf("baseline install: %v", err)
	}

	// A new pin whose download is broken must fail without touching the old file.
	badPin := Pin{Asset: "fixture.bin", SHA256: strings.Repeat("b", 64)}
	if _, err := Install(dataDir, badPin, "u", "", func(dst, url, proxy string) error {
		return os.WriteFile(dst, []byte("wrong"), 0o700)
	}); err == nil {
		t.Fatal("replacement with a bad digest was accepted")
	}
	raw, readErr := os.ReadFile(installedPath(dataDir))
	if readErr != nil {
		t.Fatalf("previously verified binary disappeared: %v", readErr)
	}
	if string(raw) != string(goodBytes) {
		t.Fatal("previously verified binary was overwritten by a rejected download")
	}
}

func TestInstallReportsDownloadFailureReadably(t *testing.T) {
	dataDir := t.TempDir()
	pin := Pin{Asset: "fixture.bin", SHA256: strings.Repeat("c", 64)}
	_, err := Install(dataDir, pin, "https://example.invalid/x", "", func(dst, url, proxy string) error {
		return fmt.Errorf("download failed: HTTP 404 from %s", url)
	})
	if err == nil {
		t.Fatal("Install ignored a download failure")
	}
	if errors.Is(err, errDigestMismatch) {
		t.Fatal("a download failure was misreported as a digest mismatch")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("the download failure reason was lost: %v", err)
	}
}

func TestVerifiedPathReportsFalseForWrongDigest(t *testing.T) {
	dataDir := t.TempDir()
	content := []byte("real bytes")
	digest := writeFixture(t, installedPath(dataDir), content)

	if _, ok := VerifiedPath(dataDir, Pin{SHA256: strings.Repeat("d", 64)}); ok {
		t.Fatal("VerifiedPath accepted a file whose digest is not the pin")
	}
	if _, ok := VerifiedPath(dataDir, Pin{SHA256: digest}); !ok {
		t.Fatal("VerifiedPath rejected a file whose digest IS the pin")
}
}

func TestVerifiedPathReportsFalseWhenMissing(t *testing.T) {
	dataDir := t.TempDir()
	if path, ok := VerifiedPath(dataDir, Pin{SHA256: strings.Repeat("e", 64)}); ok {
		t.Fatalf("VerifiedPath claimed %q exists", path)
	}
}

func TestFileSHA256MatchesForKnownBytes(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "x")
	want := writeFixture(t, path, []byte("hello"))
	got, err := FileSHA256(path)
	if err != nil {
		t.Fatalf("FileSHA256: %v", err)
	}
	if got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
}
