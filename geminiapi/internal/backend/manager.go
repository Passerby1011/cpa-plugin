package backend

// Manager: the supervised lifecycle of ONE managed backend.
//
// # One Manager, one flavor
//
// A Manager is constructed with a Flavor and is permanently bound to it: its
// pin, its data directory, its port, its pid file, its process group and its
// health probe all come from that flavor. Two namespaces are therefore two
// Managers, and neither can answer for the other — which is the whole reason
// Flavor exists rather than a shared supervisor with a mode flag.
//
// # States and why they matter
//
//	stopped  – nothing is listening and nothing of ours is running
//	starting – spawn in progress / health not yet green
//	running  – health probe is green with this setup's API key
//	foreign  – something answers on the recorded port that is NOT our backend
//	failed   – install or start failed; LastError says why
//
// "foreign" is deliberately sticky: when another service owns the port, the
// manager refuses to start, refuses to stop, and offers a port change. Guessing
// would either kill a stranger's process or silently re-point the plugin at it.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State is the lifecycle state of a managed backend.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateForeign  State = "foreign"
	StateFailed   State = "failed"
)

// Status is the operator-facing snapshot. It must never contain a cookie, an
// API key, or an admin credential: it is rendered in a page and included in a
// Management API response.
type Status struct {
	State State `json:"state"`
	// Flavor names the backend this snapshot describes.
	Flavor FlavorID `json:"flavor"`
	// Managed is false when the namespace points at an external backend, in
	// which case this plugin owns no process.
	Managed bool `json:"managed"`
	// Version is the pinned release version.
	Version string `json:"version,omitempty"`
	// BinaryName is the executable this manager supervises.
	BinaryName string `json:"binary_name,omitempty"`
	// PinnedSHA256 is the digest the launch path must match (truncated).
	PinnedSHA256 string `json:"pinned_sha256,omitempty"`
	// BinaryPresent reports whether a digest-verified binary exists.
	BinaryPresent bool `json:"binary_present"`
	// DataDir is where the state lives.
	DataDir string `json:"data_dir"`
	// Origin is the scheme://host:port the backend serves.
	Origin string `json:"origin,omitempty"`
	// Port is the recorded port.
	Port int `json:"port"`
	// PID is this run's process id, 0 when unknown.
	PID int `json:"pid"`
	// PIDVerified reports whether that PID is running our binary.
	PIDVerified bool `json:"pid_verified"`
	// Health is the last probe result.
	Health Health `json:"health"`
	// Accounts are pool counts read from the backend's own admin surface
	// (counts only, never identifiers or cookies).
	Accounts AccountCounts `json:"accounts"`
	// LastError is a secret-free failure description.
	LastError string `json:"last_error,omitempty"`
	// StartedAt is when this run spawned the process.
	StartedAt string `json:"started_at,omitempty"`
	// Restarts counts crash restarts performed in this run.
	Restarts int `json:"restarts"`
}

// AccountCounts is the pool summary the panel may show.
type AccountCounts struct {
	Total   int `json:"total"`
	Enabled int `json:"enabled"`
	// Known is false when the count could not be read (external backend,
	// admin API disabled, or a backend that does not expose it).
	Known bool `json:"known"`
	// Error is why the count is unknown, already redacted.
	Error string `json:"error,omitempty"`
}

// Manager owns one managed backend's lifecycle. It is safe for concurrent use:
// the Management API and the request path both drive it.
type Manager struct {
	flavor        Flavor
	dataDir       string
	preferredPort int
	syncProxy     string
	modelTimeout  time.Duration
	startTimeout  time.Duration
	doer          HTTPDoer
	download      Downloader
	downloadURL   string

	mu        sync.Mutex
	spawned   *Spawned
	setup     Setup
	state     State
	lastErr   string
	pid       int
	restarts  int
	startedAt time.Time
	// foreignPID is a live process we refused to adopt because its identity
	// check failed, kept only for diagnostics.
	foreignPID int
	// stopRequested suppresses the crash-restart loop.
	stopRequested bool
	// watcher manages the crash-restart goroutine.
	watcherStop chan struct{}
	watcherDone chan struct{}
}

// Options configures a Manager.
type Options struct {
	// Flavor selects the backend. It is required: a Manager with no flavor
	// would have to guess which binary, ports and files it owns.
	Flavor Flavor
	// DataDir is this flavor's OWN data directory.
	DataDir       string
	PreferredPort int
	SyncProxy     string
	ModelTimeout  time.Duration
	StartTimeout  time.Duration
	Doer          HTTPDoer
	Download      Downloader
	// DownloadURL overrides the pinned URL. Only a test sets it; production
	// always derives the URL from the pin.
	DownloadURL string
}

// New returns a Manager bound to one flavor. Nothing is downloaded or spawned
// here: the Manager reads existing state and reports it.
func New(opts Options) *Manager {
	if !opts.Flavor.Valid() {
		return &Manager{
			flavor: opts.Flavor,
			state:  StateFailed,
			lastErr: fmt.Sprintf("no backend flavor configured: %q is not a flavor this plugin can supervise",
				opts.Flavor.ID),
			dataDir: opts.DataDir,
		}
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 30 * time.Second
	}
	if opts.ModelTimeout <= 0 {
		opts.ModelTimeout = DefaultModelTimeout
	}
	m := &Manager{
		flavor:        opts.Flavor,
		dataDir:       opts.DataDir,
		preferredPort: opts.PreferredPort,
		syncProxy:     opts.SyncProxy,
		modelTimeout:  opts.ModelTimeout,
		startTimeout:  opts.StartTimeout,
		doer:          opts.Doer,
		download:      opts.Download,
		downloadURL:   opts.DownloadURL,
		state:         StateStopped,
	}
	return m
}

// Flavor returns the backend this manager supervises.
func (m *Manager) Flavor() Flavor { return m.flavor }

// FlavorID returns the backend id, which equals the model namespace it serves.
func (m *Manager) FlavorID() FlavorID { return m.flavor.ID }

// DataDir returns the managed data directory.
func (m *Manager) DataDir() string { return m.dataDir }

// Setup returns the last known setup. ok is false before an install.
func (m *Manager) Setup() (Setup, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setup.BaseURL == "" {
		return Setup{}, false
	}
	return m.setup, true
}

// CurrentState reports the state without probing.
func (m *Manager) CurrentState() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// StatusSnapshot probes the backend (if a setup exists) and returns a
// secret-free Status. It never starts anything.
func (m *Manager) StatusSnapshot(ctx context.Context) Status {
	m.mu.Lock()
	setup, ok := m.setup, m.setup.BaseURL != ""
	spawned := m.spawned
	state := m.state
	lastErr := m.lastErr
	pid := m.pid
	startedAt := m.startedAt
	restarts := m.restarts
	m.mu.Unlock()

	layout := m.flavor.Layout(m.dataDir)
	st := Status{
		State:      state,
		Flavor:     m.flavor.ID,
		Managed:    true,
		Version:    m.flavor.ReleaseVersion,
		BinaryName: m.flavor.BinaryName,
		DataDir:    m.dataDir,
		LastError:  lastErr,
		Restarts:   restarts,
	}
	if digest, err := m.flavor.ExpectedBinaryDigest(platformGOOS(), platformGOARCH()); err == nil {
		st.PinnedSHA256 = RedactHashes(digest)
	} else {
		st.LastError = err.Error()
		st.State = StateFailed
	}
	if _, verified := m.flavor.VerifiedPath(m.dataDir); verified {
		st.BinaryPresent = true
	}
	if !ok {
		return st
	}
	st.Origin = setup.Origin
	st.Port = setup.Port()
	st.PID = pid
	if spawned != nil && spawned.PID > 0 {
		st.PIDVerified = VerifyProcessIdentity(spawned.PID, layout.Binary)
	}
	if !startedAt.IsZero() {
		st.StartedAt = startedAt.Format(time.RFC3339)
	}
	if m.doer == nil {
		st.Health = Health{State: HealthDown, Reason: "no HTTP transport available"}
		return st
	}
	health := Probe(ctx, m.doer, setup)
	st.Health = health
	// Keep the state honest: a probe that finds nothing means stopped, whatever
	// the last action claimed.
	derived := state
	switch health.State {
	case HealthUp:
		if spawned == nil {
			// Something answering with our key but not our child: a backend
			// left by a previous run, which the operator may adopt.
			derived = StateRunning
		}
	case HealthDown:
		if spawned == nil {
			derived = StateStopped
		}
	case HealthForeign:
		if spawned == nil {
			derived = StateForeign
		}
	}
	if derived != state {
		m.mu.Lock()
		m.state = derived
		m.mu.Unlock()
		st.State = derived
	}
	st.Accounts = m.accountCounts(ctx, setup)
	return st
}

// accountCounts reads the pool summary. A failure yields Known=false with a
// redacted reason; it never fails the whole status call.
func (m *Manager) accountCounts(ctx context.Context, setup Setup) AccountCounts {
	if m.doer == nil || !setup.HasAdminCredentials() {
		return AccountCounts{Error: "the backend's admin surface is not reachable through this plugin (external backend, or no admin credentials)"}
	}
	accounts, err := ReadAccountCounts(ctx, m.doer, setup)
	if err != nil {
		return AccountCounts{Error: "the backend did not report its account pool: " + RedactText(err.Error(), 180)}
	}
	return accounts
}

// Install downloads and verifies the pinned binary. It does NOT start it.
func (m *Manager) Install(ctx context.Context) (string, error) {
	if !m.flavor.Valid() {
		return "", m.fail(errors.New("no backend flavor configured"))
	}
	if err := EnsureDataDir(m.dataDir); err != nil {
		return "", m.fail(err)
	}
	url := m.downloadURL
	if url == "" {
		pin, err := m.flavor.CurrentPin()
		if err != nil {
			return "", m.fail(err)
		}
		url = m.flavor.ReleaseBaseURL() + pin.Asset
	}
	path, err := m.flavor.Install(m.dataDir, url, m.syncProxy, m.download)
	if err != nil {
		return "", m.fail(err)
	}
	return path, nil
}

// EnsureBackend makes a verified binary present and the backend running, and
// returns the setup a caller can talk to. It is the function the executor calls
// before every request when this namespace is managed.
//
// Sequence:
//  1. an already-green backend with this setup's key is returned as-is;
//  2. a leftover process from a previous run is reclaimed ONLY after a
//     PID+path identity check;
//  3. a foreign listener on the recorded port -> pick another port and persist;
//  4. the binary is installed if missing or not the pin (digest verified);
//  5. spawn, then poll health until green or the start deadline lapses.
func (m *Manager) EnsureBackend(ctx context.Context) (Setup, error) {
	if !m.flavor.Valid() {
		return Setup{}, m.fail(errors.New("no backend flavor configured"))
	}
	m.mu.Lock()
	if m.state == StateStarting {
		m.mu.Unlock()
		return Setup{}, errors.New("the local backend is already starting")
	}
	m.state = StateStarting
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.state == StateStarting {
			m.state = StateStopped
		}
		m.mu.Unlock()
	}()

	layout := m.flavor.Layout(m.dataDir)
	if err := EnsureDataDir(m.dataDir); err != nil {
		return Setup{}, m.fail(err)
	}
	if _, err := m.flavor.CurrentPin(); err != nil {
		return Setup{}, m.fail(err)
	}

	// 1. Something already answering with our key: adopt it. This is the
	// normal case after a plugin reload — the process is still running.
	if setup, ok := m.flavor.ReadSetup(m.dataDir); ok {
		if m.doer != nil {
			if health := Probe(ctx, m.doer, setup); health.State == HealthUp {
				m.mu.Lock()
				m.setup = setup
				m.state = StateRunning
				m.lastErr = ""
				m.mu.Unlock()
				return setup, nil
			}
		}
	}

	// 2. Reclaim a leftover process from a previous run, with an identity check.
	if _, verified := m.flavor.VerifiedPath(m.dataDir); verified {
		if report := ReclaimStale(m.dataDir, layout.Binary); report.Recorded > 0 && report.Alive && !report.Ours {
			// A live PID that is not ours: record it, never kill it.
			m.mu.Lock()
			m.foreignPID = report.Recorded
			m.mu.Unlock()
		}
	}

	// 3. Port selection. An existing config keeps its port unless something
	// foreign occupies it.
	port := m.preferredPort
	if port <= 0 {
		port = m.flavor.DefaultPort
	}
	if existing, ok := m.flavor.ReadSetup(m.dataDir); ok {
		port = existing.Port()
	}
	if m.doer != nil && PortInUse(port) {
		// Occupied: is it OUR backend (key accepted) or a stranger?
		trySetup := Setup{
			Origin:  fmt.Sprintf("http://127.0.0.1:%d", port),
			BaseURL: fmt.Sprintf("http://127.0.0.1:%d/v1", port),
			Flavor:  m.flavor.ID,
		}
		if saved, ok := m.flavor.ReadSetup(m.dataDir); ok && saved.Port() == port {
			trySetup.APIKey = saved.APIKey
			trySetup.AdminToken = saved.AdminToken
			trySetup.AdminUser = saved.AdminUser
			trySetup.AdminPassword = saved.AdminPassword
		}
		health := Probe(ctx, m.doer, trySetup)
		if health.State == HealthForeign {
			next, perr := PickPort(port + 1)
			if perr != nil {
				return Setup{}, m.fail(perr)
			}
			if serr := m.flavor.SetPort(m.dataDir, next); serr != nil {
				return Setup{}, m.fail(serr)
			}
			port = next
		}
	}

	// 4. Ensure the data files exist, then the binary.
	setup, err := m.flavor.EnsureDataFiles(m.dataDir, port)
	if err != nil {
		return Setup{}, m.fail(err)
	}
	url := m.downloadURL
	if url == "" {
		pin, perr := m.flavor.CurrentPin()
		if perr != nil {
			return Setup{}, m.fail(perr)
		}
		url = m.flavor.ReleaseBaseURL() + pin.Asset
	}
	if _, err := m.flavor.Install(m.dataDir, url, m.syncProxy, m.download); err != nil {
		return Setup{}, m.fail(err)
	}
	if err := ValidateBindTarget("127.0.0.1", portFromOrigin(setup.Origin)); err != nil {
		return Setup{}, m.fail(err)
	}

	// 5. Spawn and wait for green.
	args, err := m.flavor.LaunchArgs(m.dataDir)
	if err != nil {
		return Setup{}, m.fail(err)
	}
	spawned, err := Spawn(SpawnOptions{
		BinaryPath: layout.Binary,
		Args:       args,
		Env:        m.flavor.LaunchEnv(setup, nil),
		Dir:        m.dataDir,
		LogPath:    layout.LogFile,
	})
	if err != nil && spawned == nil {
		return Setup{}, m.fail(err)
	}
	if err != nil {
		// Spawned but not adoptable into a job/group: the process exists, so
		// record it before reporting the weakened guarantee.
		_ = WritePIDFile(m.dataDir, spawned.PID)
		return Setup{}, m.fail(err)
	}
	if werr := WritePIDFile(m.dataDir, spawned.PID); werr != nil {
		_ = spawned.Terminate()
		return Setup{}, m.fail(fmt.Errorf("record backend pid: %w", werr))
	}

	m.mu.Lock()
	m.spawned = spawned
	m.setup = setup
	m.pid = spawned.PID
	m.startedAt = time.Now()
	m.mu.Unlock()

	if m.doer == nil {
		m.mu.Lock()
		m.state = StateRunning
		m.mu.Unlock()
		return setup, nil
	}

	deadline := time.Now().Add(m.startTimeout)
	for time.Now().Before(deadline) {
		health := Probe(ctx, m.doer, setup)
		if health.State == HealthUp && health.APIKeyOK {
			m.mu.Lock()
			m.state = StateRunning
			m.lastErr = ""
			m.mu.Unlock()
			m.watchForCrash()
			return setup, nil
		}
		if health.State == HealthForeign {
			_ = spawned.Terminate()
			return Setup{}, m.fail(fmt.Errorf("%s answers with another service's key; the backend may have bound the wrong port — see %s", setup.Origin, layout.LogFile))
		}
		select {
		case <-ctx.Done():
			_ = spawned.Terminate()
			return Setup{}, m.fail(ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
	_ = spawned.Terminate()
	removePIDFile(m.dataDir)
	return Setup{}, m.fail(fmt.Errorf("the %s backend did not answer within %s; see %s",
		m.flavor.Label(), m.startTimeout, layout.LogFile))
}

// Start installs-if-needed and starts the backend, returning its status.
func (m *Manager) Start(ctx context.Context) (Status, error) {
	m.mu.Lock()
	m.stopRequested = false
	m.mu.Unlock()
	if _, err := m.EnsureBackend(ctx); err != nil {
		return m.StatusSnapshot(ctx), err
	}
	return m.StatusSnapshot(ctx), nil
}

// Stop stops the supervised backend and everything in its process group.
//
// It refuses to touch a process whose identity does not verify, and it refuses
// entirely when the recorded port is occupied by something foreign: in that
// case the operator must choose a port, not have the plugin kill a stranger.
func (m *Manager) Stop(ctx context.Context) (Status, error) {
	m.mu.Lock()
	m.stopRequested = true
	m.closeWatcher()
	spawned := m.spawned
	m.mu.Unlock()

	layout := m.flavor.Layout(m.dataDir)
	if spawned == nil {
		// No child in this run. A previous run's process may still be alive: stop
		// it with the same identity check.
		if _, verified := m.flavor.VerifiedPath(m.dataDir); verified {
			if report := ReclaimStale(m.dataDir, layout.Binary); report.Recorded > 0 && report.Alive && !report.Ours {
				return m.StatusSnapshot(ctx), fmt.Errorf("cannot stop unknown process on pid %d: it does not match this backend's binary", report.Recorded)
			}
		}
		if setup, ok := m.flavor.ReadSetup(m.dataDir); ok && m.doer != nil {
			if health := Probe(ctx, m.doer, setup); health.State == HealthForeign {
				return m.StatusSnapshot(ctx), fmt.Errorf("port %d is owned by another service; refusing to stop it — change the port for namespace %s",
					setup.Port(), m.flavor.ID)
			}
		}
		m.mu.Lock()
		m.state = StateStopped
		m.mu.Unlock()
		return m.StatusSnapshot(ctx), nil
	}

	err := spawned.Terminate()
	m.mu.Lock()
	m.spawned = nil
	m.pid = 0
	m.state = StateStopped
	m.startedAt = time.Time{}
	if err != nil {
		m.lastErr = err.Error()
	}
	m.mu.Unlock()
	removePIDFile(m.dataDir)
	return m.StatusSnapshot(ctx), err
}

// Shutdown is Stop without a caller context, for the plugin shutdown export.
func (m *Manager) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), terminateGrace+5*time.Second)
	defer cancel()
	_, _ = m.Stop(ctx)
}

// waitForCrashExit reports when the supervised child exits on its own and
// whether it exited cleanly. It drives the restart loop.
func (m *Manager) waitForCrashExit(sp *Spawned) (bool, bool) {
	if sp == nil || sp.cmd == nil || sp.cmd.ProcessState != nil {
		return true, false
	}
	_ = sp.cmd.Wait()
	status := sp.cmd.ProcessState
	if status == nil {
		return true, false
	}
	return true, status.Success()
}

// maxCrashRestarts bounds automatic restarts. A backend that keeps dying is a
// configuration problem (port, missing account, broken binary) and spinning on
// it would hide the cause.
const maxCrashRestarts = 3

// watchForCrash starts the crash-restart loop for the current child.
//
// The loop is what makes a crashed backend recover without an operator: the
// child's exit is awaited, and unless Stop was requested the backend is
// re-spawned (up to maxCrashRestarts) and health-probed again. Every attempt is
// recorded in Restarts so the panel shows that it happened.
func (m *Manager) watchForCrash() {
	m.mu.Lock()
	spawned := m.spawned
	if spawned == nil || spawned.cmd == nil || m.watcherStop != nil {
		m.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	m.watcherStop, m.watcherDone = stop, done
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()

	go func() {
		defer close(done)
		for {
			if _, _ = m.waitForCrashExit(spawned); false {
				return
			}
			m.mu.Lock()
			stopping := m.stopRequested
			attempts := m.restarts
			m.mu.Unlock()
			if stopping {
				return
			}
			if attempts >= maxCrashRestarts {
				m.mu.Lock()
				m.state = StateFailed
				m.lastErr = fmt.Sprintf("the backend exited %d times without being stopped; see %s", attempts, m.LogFile())
				m.mu.Unlock()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}

			if !m.flavor.Valid() {
				return
			}
			layout := m.flavor.Layout(m.dataDir)
			savedSetup, okSetup := m.flavor.ReadSetup(m.dataDir)
			if !okSetup {
				m.fail(errors.New("the backend's config disappeared; refusing to restart"))
				return
			}
			setup, errSetup := m.flavor.EnsureDataFiles(m.dataDir, savedSetup.Port())
			if errSetup != nil {
				m.fail(errSetup)
				return
			}
			args, errArgs := m.flavor.LaunchArgs(m.dataDir)
			if errArgs != nil {
				m.fail(errArgs)
				return
			}
			if _, verified := m.flavor.VerifiedPath(m.dataDir); !verified {
				m.fail(fmt.Errorf("backend binary no longer matches its pin; refusing to restart"))
				return
			}
			next, errSpawn := Spawn(SpawnOptions{
				BinaryPath: layout.Binary,
				Args:       args,
				Env:        m.flavor.LaunchEnv(setup, nil),
				Dir:        m.dataDir,
				LogPath:    layout.LogFile,
			})
			if errSpawn != nil {
				m.fail(errSpawn)
				return
			}
			_ = WritePIDFile(m.dataDir, next.PID)

			m.mu.Lock()
			m.spawned = next
			m.pid = next.PID
			m.restarts++
			m.startedAt = time.Now()
			m.mu.Unlock()
			spawned = next

			if m.doer != nil {
				deadline := time.Now().Add(m.startTimeout)
				healthy := false
				for time.Now().Before(deadline) {
					if Probe(ctx, m.doer, setup).State == HealthUp {
						healthy = true
						break
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(300 * time.Millisecond):
					}
				}
				if !healthy {
					_ = next.Terminate()
					m.fail(fmt.Errorf("the restarted backend did not answer within %s; see %s", m.startTimeout, layout.LogFile))
					return
				}
			}
			m.mu.Lock()
			m.state = StateRunning
			m.lastErr = ""
			m.mu.Unlock()
		}
	}()
}

// closeWatcher signals the crash watcher, if any, to exit.
func (m *Manager) closeWatcher() {
	if m.watcherStop == nil {
		return
	}
	close(m.watcherStop)
	m.watcherStop = nil
	m.watcherDone = nil
}

// fail records a secret-free error and returns it.
func (m *Manager) fail(err error) error {
	if err == nil {
		return nil
	}
	m.mu.Lock()
	m.state = StateFailed
	m.lastErr = err.Error()
	m.mu.Unlock()
	return err
}

// portFromOrigin extracts the port of an http origin.
func portFromOrigin(origin string) int {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	idx := strings.LastIndex(trimmed, ":")
	if idx < 0 {
		return DefaultPortValue
	}
	port := 0
	for _, r := range trimmed[idx+1:] {
		if r < '0' || r > '9' {
			break
		}
		port = port*10 + int(r-'0')
	}
	if port <= 0 {
		return DefaultPortValue
	}
	return port
}

// LogFile is this manager's backend log path (used in operator-facing errors).
func (m *Manager) LogFile() string {
	return filepath.Join(m.dataDir, LogFileName)
}
