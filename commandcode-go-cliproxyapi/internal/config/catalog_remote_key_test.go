package config

import "testing"

// TestCatalogRemoteAcceptsTheFlatKeyThePanelWrites pins the bug that made a
// configured remote model source silently do nothing.
//
// The management UI stores each ConfigField under its own top-level key
// ("Name is the configuration key under plugins.configs.<pluginID>"), so the
// panel writes a flat `catalog-remote:`. The parser originally only read the
// nested `catalog: { remote: }` spelling, so the URL was dropped, the remote
// fetch never ran, and a go-cli-only pool ended up with an empty model list —
// with no error to explain it.
func TestCatalogRemoteAcceptsTheFlatKeyThePanelWrites(t *testing.T) {
	const url = "https://raw.githubusercontent.com/MAXeaglet/commandcode-proxy/master/proxy.mjs"

	cfg, err := Load([]byte("catalog-remote: " + url + "\n" + withKey))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Catalog.Remote != url {
		t.Fatalf("flat catalog-remote was dropped: got %q, want %q", cfg.Catalog.Remote, url)
	}

	// The nested spelling must keep working: it is what a hand-written config
	// file uses, and it is documented in the README.
	nested, err := Load([]byte("catalog:\n  remote: " + url + "\n" + withKey))
	if err != nil {
		t.Fatalf("Load(nested): %v", err)
	}
	if nested.Catalog.Remote != url {
		t.Fatalf("nested catalog.remote was dropped: got %q", nested.Catalog.Remote)
	}

	// When both are present the flat key wins: that is the one the panel owns,
	// so an operator editing the UI must be able to override a stale file value.
	both, err := Load([]byte(
		"catalog-remote: " + url + "\n" +
			"catalog:\n  remote: https://example.test/other.mjs\n" + withKey))
	if err != nil {
		t.Fatalf("Load(both): %v", err)
	}
	if both.Catalog.Remote != url {
		t.Fatalf("flat key should win when both are set: got %q", both.Catalog.Remote)
	}
}

// TestCatalogRemoteAbsentStaysEmpty pins that nothing is invented when the key
// is unset — an empty remote must not default to some URL and start fetching.
func TestCatalogRemoteAbsentStaysEmpty(t *testing.T) {
	cfg, err := Load([]byte(withKey))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Catalog.Remote != "" {
		t.Fatalf("remote defaulted to %q, want empty", cfg.Catalog.Remote)
	}
}

// TestFlatConfigKeysMatchPanelFieldNames keeps the panel's ConfigField names and
// the accepted YAML keys in step. These three were already flat; only
// catalog-remote was ever wrong, and this test stops a future field from
// repeating that mistake on the other side of the split.
func TestFlatConfigKeysMatchPanelFieldNames(t *testing.T) {
	cfg, err := Load([]byte(
		"max-inflight: 7\n" +
			"retry:\n  max: 5\n" +
			"watchdog:\n  stream: 45s\n" + withKey))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxInflight != 7 {
		t.Errorf("max-inflight: got %d, want 7", cfg.MaxInflight)
	}
	if cfg.Retry.Max != 5 {
		t.Errorf("retry.max: got %d, want 5", cfg.Retry.Max)
	}
	if cfg.Watchdog.Stream.String() != "45s" {
		t.Errorf("watchdog.stream: got %s, want 45s", cfg.Watchdog.Stream)
	}
}
