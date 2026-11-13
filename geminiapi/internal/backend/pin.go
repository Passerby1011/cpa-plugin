// Package backend manages the local OpenAI-compatible backend binary this
// plugin owns: pinned version + sha256, download, verification, spawn, health
// probe, crash restart, and teardown of the whole process group.
//
// # Security boundary
//
// The plugin never executes a path it has not verified. Every launch is
// preceded by a sha256 check against a compile-time pin, and every kill is
// preceded by a PID **identity** check (PID alone is not an identity: a live
// PID may belong to an unrelated process after a crash or a reboot).
//
// # Why the binary is not shipped
//
// The backend is ~40 MB per platform and the plugin is a single .so per
// platform. Shipping every platform's binary inside every .so would multiply
// the artifact size for no benefit, and bundling cookies or account state is
// out of the question. Everything here therefore runs at first use, into a
// data directory the operator can point anywhere with `data-dir`.
package backend

import (
	"fmt"
	"runtime"
	"strings"
)

// Release is the pinned backend release. These constants are the ONLY source of
// truth for what may be downloaded and executed: a manifest served over the
// network is never trusted, so a compromised release page cannot redirect the
// plugin at an unverified binary.
//
// Source: BUKOWSKIREAL/magpie-gemini-web lib/backend-release.json (commit
// 4d9c20f), tag backend-v4.28.0-magpie.1.
const (
	ReleaseVersion   = "4.28.0-magpie.1"
	ReleaseTag       = "backend-v4.28.0-magpie.1"
	ReleaseRepo      = "BUKOWSKIREAL/magpie-gemini-web"
	releaseBaseURL   = "https://github.com/" + ReleaseRepo + "/releases/download/" + ReleaseTag + "/"
	BinaryName       = "gemini-web2api-go"
	UpstreamVersion  = "4.28.0"
	ConfigFileName   = "config.json"
	SecretsFileName  = "secrets.json"
	DatabaseFileName = "gemini.db"
	PIDFileName      = "backend.pid"
	LogFileName      = "backend.log"
)

// Pin is one platform's asset and digest.
type Pin struct {
	// Asset is the release asset file name.
	Asset string
	// SHA256 is the lowercase hex digest of the exact asset bytes.
	SHA256 string
}

// Pins holds the verified asset for every platform this plugin can run on. A
// platform missing here is refused rather than guessed at.
var Pins = map[string]Pin{
	"linux/amd64":   {Asset: "gemini-web2api-go_v4.28.0-magpie.1_linux_amd64", SHA256: "ec3ee723dbd6f99b6e874eaf2aab23b7c8afb3ab5c2a3498b51fea27b1f42ded"},
	"linux/arm64":   {Asset: "gemini-web2api-go_v4.28.0-magpie.1_linux_arm64", SHA256: "bcadd1a679c36ca1eb582835c4a9d01a157612951cd9a2f58c154aa20fc2e893"},
	"darwin/amd64":  {Asset: "gemini-web2api-go_v4.28.0-magpie.1_darwin_amd64", SHA256: "7f264075c556592f9753a43358c4cf1bd90b3d41343d88836f835d20d9a70f21"},
	"darwin/arm64":  {Asset: "gemini-web2api-go_v4.28.0-magpie.1_darwin_arm64", SHA256: "9c495eb5f6e97fceb9a97fe21b4d33f1127592aa96a8206e3a243244571d9c37"},
	"windows/amd64": {Asset: "gemini-web2api-go_v4.28.0-magpie.1_windows_amd64.exe", SHA256: "8cd8f92133873fe4e15fed6081404ad609c90e8cc7701154d008e23c8b133648"},
	"windows/arm64": {Asset: "gemini-web2api-go_v4.28.0-magpie.1_windows_arm64.exe", SHA256: "f4d7d46be7fbfe12e6ae26ef3dbd861519611e5dbd364e335ab7a0facecf87ce"},
}

// PinFor returns the pin for a GOOS/GOARCH pair.
func PinFor(goos, goarch string) (Pin, error) {
	key := strings.ToLower(strings.TrimSpace(goos)) + "/" + strings.ToLower(strings.TrimSpace(goarch))
	pin, ok := Pins[key]
	if !ok {
		return Pin{}, fmt.Errorf("no pinned %s %s build for %s/%s; this plugin supports linux and windows on amd64/arm64 and darwin on amd64/arm64", BinaryName, ReleaseVersion, goos, goarch)
	}
	return pin, nil
}

// CurrentPin is PinFor this process's platform.
func CurrentPin() (Pin, error) { return PinFor(runtime.GOOS, runtime.GOARCH) }

// DownloadURL is the exact release URL for one pin. It is derived from the pin
// table, never from a response body.
func DownloadURL(pin Pin) string { return releaseBaseURL + pin.Asset }

// ReleasePageURL is the human-facing release page, used in error text so an
// operator can see what was expected.
func ReleasePageURL() string {
	return "https://github.com/" + ReleaseRepo + "/releases/tag/" + ReleaseTag
}
