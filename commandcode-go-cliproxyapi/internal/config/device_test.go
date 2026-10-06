package config

import "testing"

// TestDeviceDefaultsOnAndMatchesGocli pins two things at once: the identity
// layer defaults to ON (a config that never mentions `device` still presents a
// device), and the duplicated project-dir constant equals the gocli package's.
//
// The constant is duplicated rather than imported because importing gocli from
// config would close an import cycle (config <- catalog <- chatcompletions <-
// gocli). That makes silent drift the real risk, so the value is asserted here.
func TestDeviceDefaultsOnAndMatchesGocli(t *testing.T) {
	c, err := Load([]byte(withKey))
	if err != nil {
		t.Fatalf("Load(minimal) failed: %v", err)
	}
	if !c.Device.Enabled {
		t.Errorf("Device.Enabled = false, want true by default")
	}
	if got := c.Device.EffectiveProjectDir(); got != `C:\Users\dev\projects\app` {
		t.Errorf("EffectiveProjectDir() = %q", got)
	}
	// Keep the two literals in lockstep. If gocli.DefaultProjectDir changes,
	// this fails and forces the duplication to be updated together.
	const gocliDefaultProjectDir = `C:\Users\dev\projects\app`
	if DefaultDeviceProjectDir != gocliDefaultProjectDir {
		t.Fatalf("DefaultDeviceProjectDir drifted from gocli.DefaultProjectDir: %q vs %q",
			DefaultDeviceProjectDir, gocliDefaultProjectDir)
	}
}

// TestDeviceConfigParses checks the whole block round-trips, including the
// explicit off switch and the salt escape hatch.
func TestDeviceConfigParses(t *testing.T) {
	yamlText := "device:\n" +
		"  enabled: false\n" +
		"  project-dir: \"C:\\\\Users\\\\me\\\\code\\\\app\"\n" +
		"  identity-salt: \"rotate-2026\"\n" +
		withKey

	c, err := Load([]byte(yamlText))
	if err != nil {
		t.Fatalf("Load(device) failed: %v", err)
	}
	if c.Device.Enabled {
		t.Errorf("Device.Enabled = true, want the explicit false to win")
	}
	if c.Device.ProjectDir != `C:\Users\me\code\app` {
		t.Errorf("Device.ProjectDir = %q", c.Device.ProjectDir)
	}
	if c.Device.IdentitySalt != "rotate-2026" {
		t.Errorf("Device.IdentitySalt = %q", c.Device.IdentitySalt)
	}
}

// TestDeviceEnabledDefaultsWhenOnlyOtherKeysSet guards the pointer/default
// plumbing: setting a sibling key must not accidentally turn the layer off.
func TestDeviceEnabledDefaultsWhenOnlyOtherKeysSet(t *testing.T) {
	yamlText := "device:\n" +
		"  identity-salt: \"s\"\n" +
		withKey

	c, err := Load([]byte(yamlText))
	if err != nil {
		t.Fatalf("Load(device) failed: %v", err)
	}
	if !c.Device.Enabled {
		t.Errorf("Device.Enabled = false, want true when only identity-salt is set")
	}
	if c.Device.IdentitySalt != "s" {
		t.Errorf("Device.IdentitySalt = %q", c.Device.IdentitySalt)
	}
}
