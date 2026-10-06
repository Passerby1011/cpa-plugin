package gocli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

const (
	testKey      = "user_9f2c1e7a-4b3d-4a55-9c11-8e6f0d2b7a41"
	testKeyAlt   = "user_5b8e0d61-77aa-42f9-8f3c-1d9e4c6a2b70"
	testSaltA    = "test-salt-alpha"
	testSaltB    = "test-salt-bravo"
	testSaltNone = ""
)

var (
	hex64    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	macAddr  = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)
	hostPat  = regexp.MustCompile(`^DESKTOP-[0-9A-F]{8}$`)
	mailPat  = regexp.MustCompile(`^[a-z]+\.[0-9a-f]{6}@(gmail|outlook|qq|163)\.com$`)
	tracePat = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestFingerprintDeterministic pins the core promise: one credential always
// reports one machine. Ten derivations must be deeply equal and serialise to
// identical bytes, or the identity would flicker across restarts and requests.
func TestFingerprintDeterministic(t *testing.T) {
	first := GenerateFingerprintWithSalt(testSaltA, testKey)
	firstJSON := mustJSON(t, first)

	for i := 0; i < 10; i++ {
		got := GenerateFingerprintWithSalt(testSaltA, testKey)
		if !fingerprintsEqual(got, first) {
			t.Fatalf("call %d drifted:\n got %+v\nwant %+v", i, got, first)
		}
		if b := mustJSON(t, got); string(b) != string(firstJSON) {
			t.Fatalf("call %d JSON drifted:\n got %s\nwant %s", i, b, firstJSON)
		}
	}
}

func fingerprintsEqual(a, b Fingerprint) bool {
	return string(mustJSONPanic(a)) == string(mustJSONPanic(b))
}

func mustJSONPanic(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestFingerprintSaltSensitivity checks the two halves of the salt contract:
// a different identity salt moves the credential to another machine, while an
// unset salt is itself a stable configuration.
func TestFingerprintSaltSensitivity(t *testing.T) {
	a := GenerateFingerprintWithSalt(testSaltA, testKey)
	b := GenerateFingerprintWithSalt(testSaltB, testKey)
	if a.Thumbmark == b.Thumbmark {
		t.Fatalf("different salts produced the same thumbmark %q", a.Thumbmark)
	}

	noSalt1 := GenerateFingerprintWithSalt(testSaltNone, testKey)
	noSalt2 := GenerateFingerprintWithSalt(testSaltNone, testKey)
	if noSalt1.Thumbmark != noSalt2.Thumbmark {
		t.Fatalf("saltless derivation is not stable: %q vs %q", noSalt1.Thumbmark, noSalt2.Thumbmark)
	}
	if noSalt1.Thumbmark == a.Thumbmark || noSalt1.Thumbmark == b.Thumbmark {
		t.Fatal("saltless derivation collided with a salted one")
	}

	// The public entry point must actually read the environment, or a
	// deployment could set CC_FINGERPRINT_SALT and see no effect.
	t.Setenv(envIdentitySalt, testSaltA)
	fromEnv := GenerateFingerprint(testKey)
	if fromEnv.Thumbmark != a.Thumbmark {
		t.Fatalf("GenerateFingerprint ignored %s: %q != %q", envIdentitySalt, fromEnv.Thumbmark, a.Thumbmark)
	}
	t.Setenv(envIdentitySalt, testSaltB)
	if GenerateFingerprint(testKey).Thumbmark != b.Thumbmark {
		t.Fatalf("GenerateFingerprint did not follow %s to the second salt", envIdentitySalt)
	}
}

// TestFingerprintCrossKeyDivergence checks that two credentials are two
// machines: distinct thumbmarks, each individually stable under one salt.
func TestFingerprintCrossKeyDivergence(t *testing.T) {
	a1 := GenerateFingerprintWithSalt(testSaltA, testKey)
	a2 := GenerateFingerprintWithSalt(testSaltA, testKey)
	b1 := GenerateFingerprintWithSalt(testSaltA, testKeyAlt)
	b2 := GenerateFingerprintWithSalt(testSaltA, testKeyAlt)

	if a1.Thumbmark == b1.Thumbmark {
		t.Fatalf("distinct keys share thumbmark %q", a1.Thumbmark)
	}
	if !fingerprintsEqual(a1, a2) {
		t.Fatal("key A is not stable across calls")
	}
	if !fingerprintsEqual(b1, b2) {
		t.Fatal("key B is not stable across calls")
	}
	// Different identity salts are independent knobs per key.
	if GenerateFingerprintWithSalt(testSaltA, testKey).Thumbmark ==
		GenerateFingerprintWithSalt(testSaltB, testKeyAlt).Thumbmark {
		t.Fatal("cross-key/cross-salt collision")
	}
}

// TestFingerprintShape validates every component the upstream body carries, in
// both its hashed and its raw form.
func TestFingerprintShape(t *testing.T) {
	fp := GenerateFingerprintWithSalt(testSaltA, testKey)
	c := fp.Components

	if !hex64.MatchString(fp.Thumbmark) {
		t.Errorf("thumbmark %q is not 64 hex chars", fp.Thumbmark)
	}
	if !hex64.MatchString(c.MachineIDHash) {
		t.Errorf("machineIdHash %q is not 64 hex chars", c.MachineIDHash)
	}
	if !hex64.MatchString(c.OSUserHash) {
		t.Errorf("osUserHash %q is not 64 hex chars", c.OSUserHash)
	}
	if !hex64.MatchString(c.HostnameHash) {
		t.Errorf("hostnameHash %q is not 64 hex chars", c.HostnameHash)
	}
	if !hex64.MatchString(c.GitEmailHash) {
		t.Errorf("gitEmailHash %q is not 64 hex chars", c.GitEmailHash)
	}

	n := len(c.MacHashes)
	if n < 2 || n > 5 {
		t.Errorf("macHashes length %d outside {2,3,4,5}", n)
	}
	for i, h := range c.MacHashes {
		if !hex64.MatchString(h) {
			t.Errorf("macHashes[%d] %q is not 64 hex chars", i, h)
		}
	}

	if c.Runtime != "cli" {
		t.Errorf("runtime = %q, want cli", c.Runtime)
	}
	if c.CollectorVersion != 1 {
		t.Errorf("collectorVersion = %d, want 1", c.CollectorVersion)
	}
	if c.Platform != "win32" || c.Arch != "x64" || c.OSRelease != "10.0.22631" {
		t.Errorf("platform triad = %q/%q/%q", c.Platform, c.Arch, c.OSRelease)
	}
	if c.IsContainer {
		t.Error("isContainer should be false for the desktop profile")
	}
	if c.CPUModel == "" || c.CPUCount <= 0 {
		t.Errorf("cpu pair incomplete: %q x%d", c.CPUModel, c.CPUCount)
	}
	if c.MemGiB != 8 && c.MemGiB != 16 && c.MemGiB != 24 && c.MemGiB != 32 && c.MemGiB != 48 && c.MemGiB != 64 {
		t.Errorf("memGiB %d is not in the pool", c.MemGiB)
	}
	if c.Timezone == "" {
		t.Error("timezone is empty")
	}

	// Raw values, which the hashes above are derived from.
	id := deriveDevice(testSaltA, testKey)
	if !hostPat.MatchString(id.hostname) {
		t.Errorf("hostname %q does not match ^DESKTOP-[0-9A-F]{8}$", id.hostname)
	}
	if !mailPat.MatchString(id.gitEmail) {
		t.Errorf("gitEmail %q does not match the expected shape", id.gitEmail)
	}
	if len(id.macs) != n {
		t.Errorf("raw mac count %d disagrees with macHashes %d", len(id.macs), n)
	}
	for i, m := range id.macs {
		if !macAddr.MatchString(m) {
			t.Errorf("mac[%d] %q is not xx:xx:xx:xx:xx:xx", i, m)
		}
	}
	for i := 1; i < len(id.macs); i++ {
		if id.macs[i-1] > id.macs[i] {
			t.Errorf("macs not sorted ascending: %v", id.macs)
		}
	}
	if id.cpuCount != c.CPUCount {
		t.Errorf("cpu count disagrees between raw and hashed views: %d vs %d", id.cpuCount, c.CPUCount)
	}
}

// TestFingerprintCPUModelMatchesCount guards the pool's pairing: a model must
// never be reported with a core count that belongs to a different chip.
func TestFingerprintCPUModelMatchesCount(t *testing.T) {
	byModel := map[string]int{}
	for _, c := range cpuPool {
		byModel[c.Model] = c.Count
	}
	for i := 0; i < 50; i++ {
		key := testKey + "-" + string(rune('a'+i%26))
		c := GenerateFingerprintWithSalt(testSaltA, key).Components
		want, ok := byModel[c.CPUModel]
		if !ok {
			t.Fatalf("cpuModel %q is not in the pool", c.CPUModel)
		}
		if c.CPUCount != want {
			t.Fatalf("cpuModel %q reported count %d, pool says %d", c.CPUModel, c.CPUCount, want)
		}
	}
}

// TestFingerprintEmptyComponentOmitted pins the empty-signal contract: an empty
// value hashes to "", and "" is dropped from the JSON by omitempty rather than
// being sent as an empty string.
func TestFingerprintEmptyComponentOmitted(t *testing.T) {
	if got := fingerprintHash(""); got != "" {
		t.Fatalf("fingerprintHash(\"\") = %q, want empty", got)
	}
	if got := fingerprintHash("   "); got != "" {
		t.Fatalf("fingerprintHash(whitespace) = %q, want empty", got)
	}
	if fingerprintHash("  DESKTOP-ABCDEF12  ") != fingerprintHash("desktop-abcdef12") {
		t.Fatal("fingerprintHash is not trim+lowercase stable")
	}

	// A component that hashes to empty must vanish from the wire body.
	empty := Fingerprint{
		Thumbmark: "t",
		Components: FingerprintComponents{
			MachineIDHash: "",
			MacHashes:     nil,
			OSUserHash:    "",
			HostnameHash:  "",
			GitEmailHash:  "",
			Platform:      "win32",
			Arch:          "x64",
			OSRelease:     "10.0.22631",
			CPUModel:      "cpu",
			CPUCount:      1,
			MemGiB:        8,
			Timezone:      "UTC",
			Runtime:       "cli",
		},
	}
	body := string(mustJSON(t, empty))
	for _, field := range []string{"machineIdHash", "macHashes", "osUserHash", "hostnameHash", "gitEmailHash"} {
		if strings.Contains(body, field) {
			t.Errorf("empty %s was not omitted from %s", field, body)
		}
	}

	// And a real profile must publish every one of them.
	real := string(mustJSON(t, GenerateFingerprintWithSalt(testSaltA, testKey)))
	for _, field := range []string{"machineIdHash", "macHashes", "osUserHash", "hostnameHash", "gitEmailHash"} {
		if !strings.Contains(real, field) {
			t.Errorf("populated %s is missing from %s", field, real)
		}
	}
}

// TestThumbSeedDegradation checks the fallback ladder and the "unknown" floor.
func TestThumbSeedDegradation(t *testing.T) {
	full := thumbSeed(deviceIdentity{machineID: " mid ", macs: []string{"aa", "bb"}, hostname: "H", cpuModel: "C"})
	if full != "mid|aa,bb" {
		t.Errorf("full seed = %q, want mid|aa,bb", full)
	}
	noMid := thumbSeed(deviceIdentity{hostname: "H", cpuModel: "C"})
	if noMid != "H|C" {
		t.Errorf("mid-less seed = %q, want H|C", noMid)
	}
	if got := thumbSeed(deviceIdentity{}); got != "unknown" {
		t.Errorf("empty seed = %q, want unknown", got)
	}
}

// TestDeviceProfileIsSingleSource proves every derived field reads from the one
// profile struct: overriding the project dir moves its slug and nothing else.
func TestDeviceProfileIsSingleSource(t *testing.T) {
	t.Setenv(envProjectDir, `D:\work\my-app`)
	p := DefaultDeviceProfile()
	if p.ProjectDir != `D:\work\my-app` {
		t.Fatalf("project dir override ignored: %q", p.ProjectDir)
	}
	if slug := p.ProjectDirSlug(); slug != "d-work-my-app" {
		t.Errorf("slug = %q, want d-work-my-app", slug)
	}
	if p.Platform != "win32" || p.Arch != "x64" || p.IsContainer {
		t.Errorf("profile drifted with the override: %+v", p)
	}

	t.Setenv(envProjectDir, "")
	if got := DefaultDeviceProfile().ProjectDir; got != DefaultProjectDir {
		t.Errorf("empty override should fall back to %q, got %q", DefaultProjectDir, got)
	}
}

// TestSlugifyProjectPath covers the required cases plus the default dir.
func TestSlugifyProjectPath(t *testing.T) {
	cases := map[string]string{
		`C:\Users\dev\projects\app`: "c-users-dev-projects-app",
		"":                          "root",
		"A__B":                      "a-b",
		"/home/dev/app":             "home-dev-app",
		"  spaced  ":                "spaced",
		"///":                       "root",
	}
	for in, want := range cases {
		if got := SlugifyProjectPath(in); got != want {
			t.Errorf("SlugifyProjectPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewTraceparent checks the W3C shape and that calls do not repeat.
func TestNewTraceparent(t *testing.T) {
	a, b := NewTraceparent(), NewTraceparent()
	if !tracePat.MatchString(a) {
		t.Errorf("traceparent %q does not match the W3C shape", a)
	}
	if !tracePat.MatchString(b) {
		t.Errorf("traceparent %q does not match the W3C shape", b)
	}
	if a == b {
		t.Errorf("two traceparents collided: %q", a)
	}
}

// TestValidateAPIKey covers the accepted and rejected wrapper forms.
func TestValidateAPIKey(t *testing.T) {
	cases := map[string]string{
		"user_abc-123":              "user_abc-123",
		"Bearer user_abc":           "user_abc",
		"  user_x_y  ":              "user_x_y",
		"sk-notacommandcodekey":     "",
		"":                          "",
		"user_":                     "",
		"Bearer sk-user_embedded12": "user_embedded12",
		testKey:                     testKey,
	}
	for in, want := range cases {
		if got := ValidateAPIKey(in); got != want {
			t.Errorf("ValidateAPIKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// ─── Cross-implementation golden vectors ─────────────────────────────────────
//
// These vectors were produced by the REFERENCE implementation
// (MAXeaglet/commandcode-proxy proxy.mjs, generateFingerprint) and captured
// verbatim. They are the only thing standing between this port and a silent
// divergence: field names ("cpu" vs "cpuModel") and label formats
// ("model|cores" for the CPU pool) are part of the derived identity, so a
// plausible-looking refactor would otherwise keep every unit test green while
// quietly assigning every credential a different fake machine.
//
// If a vector fails, fix device.go — do NOT re-baseline it to whatever the
// current code happens to print.
type fpVector struct {
	key, salt     string
	thumbmark     string
	cpuModel      string
	cpuCount      int
	memGiB        int
	timezone      string
	machineIDHash string
	macHashes     []string
	osUserHash    string
	hostnameHash  string
	gitEmailHash  string
}

var fpGolden = []fpVector{
	{
		key: "user_testkey-0001", salt: "",
		thumbmark: "3ce1895a8cf8caa3d9a166fcc91a4900f4bf696785510e62924fe693e4b1bdd8",
		cpuModel:  "12th Gen Intel(R) Core(TM) i5-12400F", cpuCount: 6, memGiB: 24,
		timezone:      "America/Chicago",
		machineIDHash: "04009f1deea0762c4f64d9befc098dde0b9b5df6b6c41ab6a618055171127e8f",
		macHashes: []string{
			"34f92a7dacf5c9ce1e85149b3ca855990512c73657a27385e482c1aa090399ae",
			"955ead64bae700a640f4879ef3c1cf17150b9bbe8eb2e0577c94423798712e0e",
		},
		osUserHash:   "33df3268dfdc602495ca0d23a6d1b2b61ac808dfbc8b04fc5ade73af04a07678",
		hostnameHash: "cc17bc19f8c76b0a307f717e387c9737d535aef01ea6ff6a9e0d477b33eeecfe",
		gitEmailHash: "8c44ac5e205eb2ed031219755907c11ad21aff60ecef62c8c6161631f08b1c90",
	},
	{
		key: "user_abc", salt: "",
		thumbmark: "6dd2fbb7040fcebcff71ed78ff1e978db9193f05ea29b3fbda2c9e1c87900c52",
		cpuModel:  "13th Gen Intel(R) Core(TM) i7-13700K", cpuCount: 16, memGiB: 8,
		timezone:      "Australia/Sydney",
		machineIDHash: "71ecc517696ac16061ec104f1c9849e88b8ddcb65abac5395d0edc3a7d4e94f5",
		macHashes: []string{
			"c0bac49b82486e51f19ff06ceb75362c648db00b01ff21f8fed7b43077afe317",
			"c07e1e59150ad299751207b78030dd8438d7d9680d6dcf7d060e274923053498",
		},
		osUserHash:   "845392654d16216abd349a7bf1911584c4e74091483182f5fb6745e24d8bcabf",
		hostnameHash: "398038cca2249362d9de1f352309f9a99e29eb3ab9afc5a302c0940b2b2ec196",
		gitEmailHash: "1b55524b975948aaa8d18999a17d177b19f0c45002974308eb63a4dfd7b7e49a",
	},
	{
		key: "user_testkey-0001", salt: "saltA",
		thumbmark: "e7cf0f3bfec5002488799af9edccf0fbd5192c43d6b1522ab426e5872fa605d7",
		cpuModel:  "12th Gen Intel(R) Core(TM) i7-12650H", cpuCount: 10, memGiB: 24,
		timezone:      "America/Toronto",
		machineIDHash: "305855179ea125ed50d5e2e6cf9d053e20fd35cd336ba775c45a20dfab3476b6",
		macHashes: []string{
			"2a39f8378b0e80ed8433c63c138bb6c6d1e731bf7257a41f28ca0a7346233726",
			"9b16d064aa1b06cf42370de832a3696a30ae1364dafe99916e375f51fbb8e69b",
			"b8d1a88ca6930fcc774530baa27841839a9f99ec625476bf0f0f3b214cf859c3",
			"6614ba0a9f46e05d08c0fd23bac8ff741cc18ad601c3f2abaae78e2e53a5d15d",
			"fd783d258e4e1c77487a190886fd2519ae7008e3a5570cb5259e8a09d0d94e9b",
		},
		osUserHash:   "845392654d16216abd349a7bf1911584c4e74091483182f5fb6745e24d8bcabf",
		hostnameHash: "3064d75e97598ce4cc531f7238f1df660c2540297c8c903f961564e0bfeb94dc",
		gitEmailHash: "0202ec794d9e5c0da4173cbdd71031fa0d5a5ee26a40d19b6b27896e55d0ad09",
	},
	{
		key: "user_Zz9_-x", salt: "saltB",
		thumbmark: "3be17c08825623cfa73552a84407796e729f5f77d05303a34c86a7e45e55c600",
		cpuModel:  "12th Gen Intel(R) Core(TM) i7-12650H", cpuCount: 10, memGiB: 64,
		timezone:      "Asia/Hong_Kong",
		machineIDHash: "b8930d2af04c90014a3f054fa6d069c74e55a22d02cd126bc5ac1fd7876b4bd1",
		macHashes: []string{
			"85b86107129b74ed88a48b20cfad19c73c313f11e6976df9f7eb8ebb17266557",
			"a42a7e3b74a1c0a1ba65ac8d7be264d5df8dbf5823ce26b772f6852de62c653e",
			"c43a5363097193785d5362a114ad782d71c6a59cb45072281976a1d067116f6e",
			"77feec240b0f0db3e2bde6ad714e664ad9f136d7cdd8b7effd03db9c18d8f7f1",
			"8efb3fa8b59bbbe69be6b9aa7f58d67de991f27b2097768a800167763b1c41f4",
		},
		osUserHash:   "33df3268dfdc602495ca0d23a6d1b2b61ac808dfbc8b04fc5ade73af04a07678",
		hostnameHash: "e6def19e2868298d7bc6b0f992e814a5130ffaad183f4a5b9bd51f7ab7539ac7",
		gitEmailHash: "a501607b5973133f3495a36e9968160ed9c94b01b1c9b46e9a7621a91209f556",
	},
}

// TestGoldenVectorsMatchReference pins the whole derivation against the
// reference implementation, field by field.
func TestGoldenVectorsMatchReference(t *testing.T) {
	for _, v := range fpGolden {
		t.Run(v.key+"/"+v.salt, func(t *testing.T) {
			got := GenerateFingerprintWithSalt(v.salt, v.key)
			if got.Thumbmark != v.thumbmark {
				t.Errorf("thumbmark\n got %s\nwant %s", got.Thumbmark, v.thumbmark)
			}
			c := got.Components
			if c.CPUModel != v.cpuModel {
				t.Errorf("cpuModel got %q want %q", c.CPUModel, v.cpuModel)
			}
			if c.CPUCount != v.cpuCount {
				t.Errorf("cpuCount got %d want %d", c.CPUCount, v.cpuCount)
			}
			if c.MemGiB != v.memGiB {
				t.Errorf("memGiB got %d want %d", c.MemGiB, v.memGiB)
			}
			if c.Timezone != v.timezone {
				t.Errorf("timezone got %q want %q", c.Timezone, v.timezone)
			}
			if c.MachineIDHash != v.machineIDHash {
				t.Errorf("machineIdHash got %s want %s", c.MachineIDHash, v.machineIDHash)
			}
			if c.OSUserHash != v.osUserHash {
				t.Errorf("osUserHash got %s want %s", c.OSUserHash, v.osUserHash)
			}
			if c.HostnameHash != v.hostnameHash {
				t.Errorf("hostnameHash got %s want %s", c.HostnameHash, v.hostnameHash)
			}
			if c.GitEmailHash != v.gitEmailHash {
				t.Errorf("gitEmailHash got %s want %s", c.GitEmailHash, v.gitEmailHash)
			}
			if len(c.MacHashes) != len(v.macHashes) {
				t.Fatalf("macHashes len got %d want %d", len(c.MacHashes), len(v.macHashes))
			}
			for i := range v.macHashes {
				if c.MacHashes[i] != v.macHashes[i] {
					t.Errorf("macHashes[%d] got %s want %s", i, c.MacHashes[i], v.macHashes[i])
				}
			}
		})
	}
}
