package gocli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FingerprintSalt is the CLI's fixed root salt for the HASH stage. It is
// deliberately not configurable: it must match the value the reference
// implementation hashes with, or every component hash diverges.
const FingerprintSalt = "command-code:device-fingerprint:v1"

// DefaultProjectDir mirrors the reference proxy's fabricated project dir.
const DefaultProjectDir = `C:\Users\dev\projects\app`

const (
	// envIdentitySalt selects WHICH fake machine a credential maps to. It is
	// mixed into the digest stage only; it never reaches the hash stage (see
	// fingerprintHash), so rotating it cannot change a component's hash shape.
	envIdentitySalt = "CC_FINGERPRINT_SALT"
	// envProjectDir overrides the fabricated project directory.
	envProjectDir = "CC_DEVICE_PROJECT_DIR"
)

// DeviceProfile is the single source of truth for the machine the CLI claims
// to run on. Every derived field reads from this one struct, so the platform,
// arch, release, container flag and project dir can never disagree with each
// other.
type DeviceProfile struct {
	Platform    string // "win32"
	Arch        string // "x64"
	OSRelease   string // "10.0.22631"
	IsContainer bool   // false
	ProjectDir  string // env CC_DEVICE_PROJECT_DIR or DefaultProjectDir
}

// DefaultDeviceProfile returns the fabricated Windows desktop profile. The
// project dir is the only field a deployment may override, via
// CC_DEVICE_PROJECT_DIR.
func DefaultDeviceProfile() DeviceProfile {
	dir := strings.TrimSpace(os.Getenv(envProjectDir))
	if dir == "" {
		dir = DefaultProjectDir
	}
	return DeviceProfile{
		Platform:    "win32",
		Arch:        "x64",
		OSRelease:   "10.0.22631",
		IsContainer: false,
		ProjectDir:  dir,
	}
}

// ProjectDirSlug is the filesystem-safe form of ProjectDir, derived from the
// struct rather than from a second copy of the path.
func (p DeviceProfile) ProjectDirSlug() string {
	return SlugifyProjectPath(p.ProjectDir)
}

// DeviceEnvironment is the envelope's config.environment value. The reference
// sends the profile's platform verbatim ("win32"), NOT a "linux-x64" style
// string: an envelope that announces a different OS than the fingerprint is
// the exact self-contradiction the shared device profile exists to prevent.
func DeviceEnvironment() string {
	return DefaultDeviceProfile().Platform
}

// cpuSpec pairs a fabricated CPU model with the core count that belongs to it,
// so cpuModel and cpuCount can never be picked independently and contradict.
type cpuSpec struct {
	Model string
	Count int
}

var cpuPool = []cpuSpec{
	{"12th Gen Intel(R) Core(TM) i7-12650H", 10},
	{"12th Gen Intel(R) Core(TM) i5-12400F", 6},
	{"12th Gen Intel(R) Core(TM) i9-12900K", 16},
	{"13th Gen Intel(R) Core(TM) i7-13700K", 16},
	{"13th Gen Intel(R) Core(TM) i5-13600K", 14},
	{"13th Gen Intel(R) Core(TM) i9-13900K", 24},
	{"Intel(R) Core(TM) Ultra 7 155H", 16},
	{"Intel(R) Core(TM) Ultra 9 285H", 16},
	{"Intel(R) Core(TM) i9-14900K", 24},
	{"Intel(R) Core(TM) i7-14700K", 20},
	{"AMD Ryzen 7 7800X3D", 8},
	{"AMD Ryzen 9 7950X", 16},
	{"AMD Ryzen 5 7600", 6},
	{"AMD Ryzen 9 7900X", 12},
	{"AMD Ryzen 7 5800X3D", 8},
}

var memPool = []int{8, 16, 24, 32, 48, 64}

var timezonePool = []string{
	"America/New_York", "America/Chicago", "America/Los_Angeles", "America/Toronto",
	"Europe/London", "Europe/Berlin", "Europe/Paris", "Europe/Moscow", "Asia/Shanghai",
	"Asia/Tokyo", "Asia/Singapore", "Asia/Seoul", "Asia/Hong_Kong", "Australia/Sydney",
	"Pacific/Auckland",
}

var macCountPool = []int{2, 3, 4, 5}

var osUserPool = []string{"dev", "user", "admin", "coder", "engineer", "work"}

var mailDomainPool = []string{"gmail.com", "outlook.com", "qq.com", "163.com"}

// Label forms of the numeric pools, built once: fpPickIndex contests on strings.
var (
	cpuModelLabels = func() []string {
		labels := make([]string, len(cpuPool))
		for i, c := range cpuPool {
			// The reference contests on "model|cores", so both cpuModel and
			// cpuCount come from one pick and can never contradict.
			labels[i] = c.Model + "|" + strconv.Itoa(c.Count)
		}
		return labels
	}()
	macCountLabels = intLabels(macCountPool)
	memLabels      = intLabels(memPool)
)

// intLabels renders a numeric pool as the string labels fpPickIndex needs.
func intLabels(pool []int) []string {
	labels := make([]string, len(pool))
	for i, v := range pool {
		labels[i] = strconv.Itoa(v)
	}
	return labels
}

// fpDigest is the digest stage: sha256(identitySalt NUL apiKey NUL field).
// Every per-key value in this file is a slice of one such digest, which is what
// makes the whole profile reproducible from the key alone.
func fpDigest(identitySalt, apiKey, field string) []byte {
	h := sha256.New()
	h.Write([]byte(identitySalt))
	h.Write([]byte{0})
	h.Write([]byte(apiKey))
	h.Write([]byte{0})
	h.Write([]byte(field))
	return h.Sum(nil)
}

// fpPickIndex selects a pool entry by running a max-digest contest: each label
// is digested with the field name, and the lexicographically greatest digest
// wins. This is deterministic, and adding an item to a pool can only move an
// existing pick if the newcomer actually beats the incumbent — so extending a
// pool leaves most credentials on the machine they already had.
func fpPickIndex(identitySalt, apiKey, field string, labels []string) int {
	best := ""
	bestIdx := 0
	for i, label := range labels {
		d := string(fpDigest(identitySalt, apiKey, field+"\x00"+label))
		if i == 0 || d > best {
			best = d
			bestIdx = i
		}
	}
	return bestIdx
}

// fingerprintHash is the hash stage: a component value is lowercased, trimmed
// and hashed under the fixed FingerprintSalt. An empty (or all-whitespace)
// value hashes to "", which signals the caller to omit the field entirely
// rather than publish a hash of nothing.
func fingerprintHash(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(FingerprintSalt + "\x00" + v))
	return hex.EncodeToString(sum[:])
}

// deviceIdentity holds the raw, pre-hash values. It is kept separate from
// Fingerprint so the derivation can be tested on its unhashed form.
type deviceIdentity struct {
	machineID string
	macs      []string
	osUser    string
	hostname  string
	gitEmail  string
	cpuModel  string
	cpuCount  int
	memGiB    int
	timezone  string
}

// deriveDevice computes every raw value for one credential. identitySalt only
// shifts which fake machine the key lands on; it is not part of the hash stage.
func deriveDevice(identitySalt, apiKey string) deviceIdentity {
	mid := hex.EncodeToString(fpDigest(identitySalt, apiKey, "machineId")[:16])
	machineID := mid[0:8] + "-" + mid[8:12] + "-" + mid[12:16] + "-" + mid[16:20] + "-" + mid[20:32]

	macCount := macCountPool[fpPickIndex(identitySalt, apiKey, "macCount", macCountLabels)]
	macs := make([]string, 0, macCount)
	for i := 0; i < macCount; i++ {
		h := hex.EncodeToString(fpDigest(identitySalt, apiKey, "mac"+strconv.Itoa(i))[:6])
		macs = append(macs, h[0:2]+":"+h[2:4]+":"+h[4:6]+":"+h[6:8]+":"+h[8:10]+":"+h[10:12])
	}
	sort.Strings(macs)

	cpu := cpuPool[fpPickIndex(identitySalt, apiKey, "cpu", cpuModelLabels)]

	osUser := osUserPool[fpPickIndex(identitySalt, apiKey, "osUser", osUserPool)]
	mailDomain := mailDomainPool[fpPickIndex(identitySalt, apiKey, "mailDomain", mailDomainPool)]

	return deviceIdentity{
		machineID: machineID,
		macs:      macs,
		osUser:    osUser,
		hostname: "DESKTOP-" + strings.ToUpper(
			hex.EncodeToString(fpDigest(identitySalt, apiKey, "hostname")[:4])),
		gitEmail: osUser + "." +
			hex.EncodeToString(fpDigest(identitySalt, apiKey, "gitEmail")[:3]) + "@" + mailDomain,
		cpuModel: cpu.Model,
		cpuCount: cpu.Count,
		memGiB:   memPool[fpPickIndex(identitySalt, apiKey, "mem", memLabels)],
		timezone: timezonePool[fpPickIndex(identitySalt, apiKey, "timezone", timezonePool)],
	}
}

// thumbSeed assembles the machine-signal string the thumbmark hashes. It
// degrades in a fixed order: when there is no machine id at all, hostname and
// then cpuModel stand in, so the seed is never accidentally empty.
func thumbSeed(id deviceIdentity) string {
	mid := strings.TrimSpace(id.machineID)
	parts := make([]string, 0, 4)
	if mid != "" {
		parts = append(parts, mid)
	}
	if len(id.macs) > 0 {
		parts = append(parts, strings.Join(id.macs, ","))
	}
	if mid == "" {
		if id.hostname != "" {
			parts = append(parts, id.hostname)
		}
		if id.cpuModel != "" {
			parts = append(parts, id.cpuModel)
		}
	}
	seed := strings.Join(parts, "|")
	if seed == "" {
		seed = "unknown"
	}
	return seed
}

// thumbmark reduces the seed to one hash. "machine" is a fixed domain tag so a
// machine thumbmark can never collide with a component hash of the same text.
func thumbmark(id deviceIdentity) string {
	sum := sha256.Sum256([]byte(FingerprintSalt + "\x00machine\x00" + thumbSeed(id)))
	return hex.EncodeToString(sum[:])
}

// Fingerprint is the device body the CLI reports upstream.
type Fingerprint struct {
	Thumbmark  string                `json:"thumbmark"`
	Components FingerprintComponents `json:"components"`
}

// FingerprintComponents carries the individual signals behind Thumbmark.
type FingerprintComponents struct {
	MachineIDHash    string   `json:"machineIdHash,omitempty"`
	MacHashes        []string `json:"macHashes,omitempty"`
	OSUserHash       string   `json:"osUserHash,omitempty"`
	HostnameHash     string   `json:"hostnameHash,omitempty"`
	GitEmailHash     string   `json:"gitEmailHash,omitempty"`
	Platform         string   `json:"platform"`
	Arch             string   `json:"arch"`
	OSRelease        string   `json:"osRelease"`
	CPUModel         string   `json:"cpuModel"`
	CPUCount         int      `json:"cpuCount"`
	MemGiB           int      `json:"memGiB"`
	IsContainer      bool     `json:"isContainer"`
	Timezone         string   `json:"timezone"`
	Runtime          string   `json:"runtime"`          // always "cli"
	CollectorVersion int      `json:"collectorVersion"` // always 1
}

// GenerateFingerprint derives the stable fake device for one API key, using the
// process-wide CC_FINGERPRINT_SALT (empty when unset).
func GenerateFingerprint(apiKey string) Fingerprint {
	return GenerateFingerprintWithSalt(os.Getenv(envIdentitySalt), apiKey)
}

// GenerateFingerprintWithSalt is the testable core of GenerateFingerprint. The
// same (identitySalt, apiKey) pair always yields an identical Fingerprint.
func GenerateFingerprintWithSalt(identitySalt, apiKey string) Fingerprint {
	id := deriveDevice(identitySalt, apiKey)
	profile := DefaultDeviceProfile()

	macHashes := make([]string, 0, len(id.macs))
	for _, m := range id.macs {
		if h := fingerprintHash(m); h != "" {
			macHashes = append(macHashes, h)
		}
	}

	return Fingerprint{
		Thumbmark: thumbmark(id),
		Components: FingerprintComponents{
			MachineIDHash:    fingerprintHash(id.machineID),
			MacHashes:        macHashes,
			OSUserHash:       fingerprintHash(id.osUser),
			HostnameHash:     fingerprintHash(id.hostname),
			GitEmailHash:     fingerprintHash(id.gitEmail),
			Platform:         profile.Platform,
			Arch:             profile.Arch,
			OSRelease:        profile.OSRelease,
			CPUModel:         id.cpuModel,
			CPUCount:         id.cpuCount,
			MemGiB:           id.memGiB,
			IsContainer:      profile.IsContainer,
			Timezone:         id.timezone,
			Runtime:          "cli",
			CollectorVersion: 1,
		},
	}
}

var slugSeparator = regexp.MustCompile(`[^a-z0-9]+`)

// SlugifyProjectPath lowercases, replaces runs of non [a-z0-9] with "-", trims
// leading/trailing "-"; returns "root" when nothing is left.
func SlugifyProjectPath(p string) string {
	s := strings.Trim(slugSeparator.ReplaceAllString(strings.ToLower(p), "-"), "-")
	if s == "" {
		return "root"
	}
	return s
}

// NewTraceparent returns a W3C traceparent: "00-" + 16 random bytes hex + "-" +
// 8 random bytes hex + "-01".
func NewTraceparent() string {
	var b [24]byte
	// crypto/rand.Read cannot fail on any supported platform; it panics on an
	// unrecoverable system source instead of returning an error.
	_, _ = rand.Read(b[:])
	return "00-" + hex.EncodeToString(b[0:16]) + "-" + hex.EncodeToString(b[16:24]) + "-01"
}

var apiKeyPattern = regexp.MustCompile(`user_[a-zA-Z0-9_-]+`)

// ValidateAPIKey extracts a CommandCode key from a bare value or from a string
// wrapped in "Bearer ..."/"sk-..."-style noise. Returns "" when no
// `user_[A-Za-z0-9_-]+` token is present.
func ValidateAPIKey(raw string) string {
	return apiKeyPattern.FindString(raw)
}
