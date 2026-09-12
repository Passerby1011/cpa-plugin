package main

import (
	"os"
	"strings"
	"testing"
)

// The logo must be served from this repo: an upstream CDN URL can vanish at
// any time, and moving the asset without updating pluginLogoURL breaks the
// panel icon silently (management clients just render their fallback glyph).
func TestPluginLogoAssetIsRepoHosted(t *testing.T) {
	const prefix = "https://raw.githubusercontent.com/hex-ci/cpa-plugin/main/qwenwork/"
	if !strings.HasPrefix(pluginLogoURL, prefix) {
		t.Fatalf("pluginLogoURL = %q, want a %s URL", pluginLogoURL, prefix)
	}
	if _, err := os.Stat(strings.TrimPrefix(pluginLogoURL, prefix)); err != nil {
		t.Fatalf("logo asset missing on disk: %v", err)
	}
}
