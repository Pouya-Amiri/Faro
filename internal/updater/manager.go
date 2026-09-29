// Package updater finds new Faro releases on GitHub and installs them in
// place: it downloads the release file that matches how this copy was
// installed, verifies it against the release's SHA256SUMS.txt, swaps it in
// and restarts Faro.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Phase is what the updater is doing.
type Phase string

const (
	PhaseIdle        Phase = "idle"
	PhaseChecking    Phase = "checking"
	PhaseDownloading Phase = "downloading"
	PhaseInstalling  Phase = "installing"
	PhaseRestarting  Phase = "restarting"
)

const (
	// initialCheckDelay leaves startup, and the network, a moment to settle.
	initialCheckDelay = 8 * time.Second
	checkInterval     = 6 * time.Hour
	// pollInterval bounds each wait, so a computer that slept through a
	// scheduled check catches up soon after it wakes.
	pollInterval = 5 * time.Minute
	checkTimeout = 20 * time.Second
)

// retryDelays space out retries after failed background checks.
var retryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// Status is the updater state the page renders.
type Status struct {
	CurrentVersion  string `json:"currentVersion"`
	Phase           Phase  `json:"phase"`
	AutomaticChecks bool   `json:"automaticChecks"`
	// CheckedAt is when GitHub last answered, in Unix milliseconds.
	CheckedAt  int64  `json:"checkedAt,omitempty"`
	CheckError string `json:"checkError,omitempty"`

	Available     bool   `json:"available"`
	LatestVersion string `json:"latestVersion,omitempty"`
	ReleaseURL    string `json:"releaseUrl"`
	ReleaseNotes  string `json:"releaseNotes,omitempty"`
	PublishedAt   int64  `json:"publishedAt,omitempty"`

	// Method is how this copy is installed; CanInstall reports whether Faro
	// can install the available update itself. Note explains what installing
	// involves, or why it has to be done by hand.
	Method     Method `json:"method"`
	CanInstall bool   `json:"canInstall"`
	Note       string `json:"note,omitempty"`

	DownloadSize int64 `json:"downloadSize,omitempty"`
	Received     int64 `json:"received"`
	// Downloaded reports a verified download that OpenDownload can show.
	Downloaded   bool   `json:"downloaded"`
	InstallError string `json:"installError,omitempty"`

	// UpdatedFrom is the previous version when this launch completed an update.
	UpdatedFrom string `json:"updatedFrom,omitempty"`
}

// Options configures a Manager.
type Options struct {
	CurrentVersion string
	// Notify receives the status after every change; download progress is
	// throttled. Calls are made one at a time from a single goroutine.
	Notify func(Status)
	// Restart quits Faro once an update is installed and the new version's
	// launch has been arranged.
	Restart func()

	// Test hooks.
	client, downloadClient *http.Client
	endpoint, assetPrefix  string
	cacheDir               string
	detect                 func() installation
	apply                  applyFunc
	now                    func() time.Time
}

// Manager checks for, downloads and installs updates.
type Manager struct {
	options   Options
	current   version
	userAgent string
	signal    chan struct{}
	done      chan struct{}

	detectOnce   sync.Once
	installation installation

	mu           sync.Mutex
	status       Status
	latest       *release
	checking     bool
	loopCancel   context.CancelFunc
	jobCancel    context.CancelFunc
	downloadPath string
	lastProgress time.Time
	closed       bool
	// unfinished is the release an earlier launch failed to install.
	unfinished string
}

func New(options Options) (*Manager, error) {
	current, err := parseVersion(options.CurrentVersion)
	if err != nil {
		return nil, fmt.Errorf("parse current version: %w", err)
	}
	if options.client == nil {
		options.client = &http.Client{Timeout: checkTimeout}
	}
	if options.downloadClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		transport.TLSHandshakeTimeout = 15 * time.Second
		options.downloadClient = &http.Client{Transport: transport}
	}
	if options.endpoint == "" {
		options.endpoint = latestAPI
	}
	if options.assetPrefix == "" {
		options.assetPrefix = assetURLPrefix
	}
	if options.cacheDir == "" {
		options.cacheDir = defaultCacheDir()
	}
	if options.detect == nil {
		options.detect = detectInstallation
	}
	if options.apply == nil {
		options.apply = applyUpdate
	}
	if options.now == nil {
		// Wall-clock time without the monotonic reading, so intervals
		// include time the computer spent asleep.
		options.now = func() time.Time { return time.Now().Round(0) }
	}
	m := &Manager{
		options:   options,
		current:   current,
		userAgent: fmt.Sprintf("Faro/%s (%s; %s)", current, runtime.GOOS, runtime.GOARCH),
		signal:    make(chan struct{}, 1),
		done:      make(chan struct{}),
		status: Status{
			CurrentVersion: current.String(),
			Phase:          PhaseIdle,
			ReleaseURL:     ReleasesURL,
			Method:         MethodManual,
		},
	}
	m.readPendingUpdate()
	go m.notifier()
	go func() {
		// Detection can run package-manager queries; keep it off startup.
		m.detect()
		cleanupInstallation()
		m.removeDownloads(func(v version) bool { return v.compare(m.current) <= 0 })
	}()
	return m, nil
}

func defaultCacheDir() string {
	if root, err := os.UserCacheDir(); err == nil {
		return filepath.Join(root, "faro", "updates")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("faro-updates-%d", os.Getuid()))
}

// Status returns the current state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// SetAutomaticChecks starts or stops the periodic background check.
func (m *Manager) SetAutomaticChecks(enabled bool) {
	m.mu.Lock()
	m.status.AutomaticChecks = enabled
	switch {
	case enabled && m.loopCancel == nil && !m.closed:
		ctx, cancel := context.WithCancel(context.Background())
		m.loopCancel = cancel
		go m.loop(ctx)
	case !enabled && m.loopCancel != nil:
		m.loopCancel()
		m.loopCancel = nil
	}
	m.mu.Unlock()
	m.wake()
}

// Check asks GitHub for the latest release now.
func (m *Manager) Check(ctx context.Context) (Status, error) {
	err := m.check(ctx)
	return m.Status(), err
}

// Install downloads, verifies and installs the available update in the
// background, then restarts Faro. Progress is reported through Notify.
func (m *Manager) Install() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.closed:
		return errors.New("Faro is shutting down")
	case m.status.Phase == PhaseDownloading || m.status.Phase == PhaseInstalling || m.status.Phase == PhaseRestarting:
		return nil
	case !m.status.Available || m.latest == nil:
		return errors.New("no update is available")
	case !m.status.CanInstall:
		return errors.New(firstNonEmpty(m.status.Note, "this update has to be installed manually"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.jobCancel = cancel
	m.status.Phase = PhaseDownloading
	m.status.Received = 0
	m.status.InstallError = ""
	go m.run(ctx, *m.latest, m.installation)
	m.wake()
	return nil
}

// Cancel stops a download in progress. Installing cannot be interrupted.
func (m *Manager) Cancel() {
	m.mu.Lock()
	cancel := m.jobCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// OpenDownload shows the verified download, for installing it by hand.
func (m *Manager) OpenDownload() error {
	m.mu.Lock()
	path := m.downloadPath
	m.mu.Unlock()
	if path == "" {
		return errors.New("the update has not been downloaded")
	}
	if _, err := os.Stat(path); err != nil {
		return errors.New("the downloaded update is no longer available")
	}
	return openDownload(path)
}

// Close stops background checks and any download.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	if m.loopCancel != nil {
		m.loopCancel()
		m.loopCancel = nil
	}
	cancel := m.jobCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	close(m.done)
}

func (m *Manager) detect() installation {
	m.detectOnce.Do(func() {
		detected := m.options.detect()
		m.mu.Lock()
		m.installation = detected
		m.status.Method = detected.method
		if !m.status.Available {
			m.status.Note = detected.note()
		}
		m.mu.Unlock()
		m.wake()
	})
	return m.installation
}

func (m *Manager) loop(ctx context.Context) {
	next := m.options.now().Add(initialCheckDelay)
	failures := 0
	for {
		wait := min(max(next.Sub(m.options.now()), 0), pollInterval)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if m.options.now().Before(next) {
			continue
		}
		if err := m.check(ctx); err != nil && ctx.Err() == nil {
			next = m.options.now().Add(retryDelays[min(failures, len(retryDelays)-1)])
			failures++
			continue
		}
		failures = 0
		next = m.options.now().Add(checkInterval)
	}
}

func (m *Manager) check(ctx context.Context) error {
	m.mu.Lock()
	if m.checking || m.closed || m.status.Phase != PhaseIdle {
		// A download or install is already under way, which a newer
		// answer could not change.
		m.mu.Unlock()
		return nil
	}
	m.checking = true
	m.status.Phase = PhaseChecking
	m.wake()
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	latest, err := fetchLatest(ctx, m.options.client, m.options.endpoint, m.userAgent, m.options.assetPrefix)
	current := m.detect()

	m.mu.Lock()
	defer m.wake()
	defer m.mu.Unlock()
	m.checking = false
	if m.status.Phase == PhaseChecking {
		m.status.Phase = PhaseIdle
	}
	if err != nil {
		m.status.CheckError = describeError(err)
		return err
	}
	m.status.CheckError = ""
	m.status.CheckedAt = m.options.now().UnixMilli()
	m.acceptRelease(latest, current)
	return nil
}

// acceptRelease records the latest release. Callers hold m.mu.
func (m *Manager) acceptRelease(latest release, current installation) {
	m.status.LatestVersion = latest.version.String()
	if latest.version.compare(m.current) <= 0 {
		m.latest = nil
		m.status.Available = false
		m.status.CanInstall = false
		m.status.ReleaseURL = ReleasesURL
		m.status.ReleaseNotes = ""
		m.status.PublishedAt = 0
		m.status.DownloadSize = 0
		m.status.Downloaded = false
		m.status.InstallError = ""
		m.status.Note = current.note()
		m.downloadPath = ""
		return
	}
	if m.latest == nil || m.latest.version.compare(latest.version) != 0 {
		// A different release: nothing downloaded or attempted applies to it.
		m.status.Downloaded = false
		m.downloadPath = ""
		// A report that the previous launch's update did not finish stays
		// only while that release is still the one on offer.
		if latest.version.String() != m.unfinished {
			m.status.InstallError = ""
		}
		m.unfinished = ""
	}
	m.latest = &latest
	m.status.Available = true
	m.status.ReleaseURL = latest.url
	m.status.ReleaseNotes = latest.notes
	m.status.PublishedAt = 0
	if !latest.published.IsZero() {
		m.status.PublishedAt = latest.published.UnixMilli()
	}
	name := current.assetName(latest.version.String())
	file, hasFile := latest.assets[name]
	_, hasSums := latest.assets[checksumsName]
	m.status.DownloadSize = file.size
	switch {
	case current.method == MethodManual:
		m.status.CanInstall = false
		m.status.Note = current.note()
	case !hasFile:
		m.status.CanInstall = false
		m.status.Note = "This release doesn't include a download for this kind of installation yet."
	case !hasSums:
		m.status.CanInstall = false
		m.status.Note = "This release has no checksum list to verify it with, so it has to be downloaded by hand."
	default:
		m.status.CanInstall = true
		m.status.Note = current.note()
	}
}

func (m *Manager) run(ctx context.Context, latest release, current installation) {
	err := m.install(ctx, latest, current)
	m.mu.Lock()
	m.jobCancel = nil
	if err != nil {
		m.status.Phase = PhaseIdle
		m.status.Received = 0
		if !errors.Is(err, context.Canceled) && !errors.Is(err, errCancelled) {
			m.status.InstallError = describeError(err)
		}
	}
	m.wake()
	m.mu.Unlock()
}

func (m *Manager) install(ctx context.Context, latest release, current installation) error {
	release := latest.version.String()
	name := current.assetName(release)
	file, ok := latest.assets[name]
	if !ok {
		return errors.New("the release has no " + name)
	}
	sums, err := fetchChecksums(ctx, m.options.client, latest.assets[checksumsName], m.userAgent)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return errors.New("the release's checksum list does not include " + name)
	}
	m.removeDownloads(func(v version) bool { return v.compare(latest.version) != 0 })
	path, err := download(ctx, m.options.downloadClient, file, want, filepath.Join(m.options.cacheDir, release), m.userAgent, m.progress)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.jobCancel = nil
	m.downloadPath = path
	m.status.Downloaded = true
	m.status.Phase = PhaseInstalling
	m.wake()
	m.mu.Unlock()

	m.writePendingUpdate(release)
	relaunch, err := m.options.apply(context.Background(), current, path, release)
	if err != nil {
		m.clearPendingUpdate()
		return err
	}
	m.mu.Lock()
	m.status.Phase = PhaseRestarting
	m.wake()
	m.mu.Unlock()
	if err := relaunch(); err != nil {
		return fmt.Errorf("Faro %s is installed but could not restart itself. Quit and reopen Faro to finish", release)
	}
	// Let the page show that Faro is restarting before it closes.
	select {
	case <-time.After(400 * time.Millisecond):
	case <-m.done:
	}
	if m.options.Restart != nil {
		m.options.Restart()
	}
	return nil
}

func (m *Manager) progress(received, total int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Received = received
	if total > 0 {
		m.status.DownloadSize = total
	}
	if now := time.Now(); now.Sub(m.lastProgress) >= 120*time.Millisecond || received == total {
		m.lastProgress = now
		m.wake()
	}
}

// wake wakes the notifier. The channel holds one pending wake-up, so
// bursts coalesce and the notifier always reports the newest status.
func (m *Manager) wake() {
	select {
	case m.signal <- struct{}{}:
	default:
	}
}

func (m *Manager) notifier() {
	for {
		select {
		case <-m.done:
			return
		case <-m.signal:
			if m.options.Notify != nil {
				m.options.Notify(m.Status())
			}
		}
	}
}

// removeDownloads deletes cached downloads whose version matches remove.
func (m *Manager) removeDownloads(remove func(version) bool) {
	entries, err := os.ReadDir(m.options.cacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if parsed, err := parseVersion(entry.Name()); err == nil && remove(parsed) {
			_ = os.RemoveAll(filepath.Join(m.options.cacheDir, entry.Name()))
		}
	}
}

// pendingUpdate records an install in progress, so the next launch can tell
// whether it finished.
type pendingUpdate struct {
	Version string `json:"version"`
	From    string `json:"from"`
}

func (m *Manager) pendingPath() string { return filepath.Join(m.options.cacheDir, "pending.json") }

func (m *Manager) writePendingUpdate(release string) {
	data, _ := json.Marshal(pendingUpdate{Version: release, From: m.current.String()})
	if os.MkdirAll(m.options.cacheDir, 0o700) == nil {
		_ = os.WriteFile(m.pendingPath(), data, 0o600)
	}
}

func (m *Manager) clearPendingUpdate() { _ = os.Remove(m.pendingPath()) }

func (m *Manager) readPendingUpdate() {
	data, err := os.ReadFile(m.pendingPath())
	if err != nil {
		return
	}
	m.clearPendingUpdate()
	var pending pendingUpdate
	if len(data) > 4096 || json.Unmarshal(data, &pending) != nil {
		return
	}
	target, err := parseVersion(pending.Version)
	if err != nil {
		return
	}
	switch compared := target.compare(m.current); {
	case compared == 0 && pending.From != "" && pending.From != m.current.String():
		m.status.UpdatedFrom = pending.From
	case compared > 0:
		m.unfinished = target.String()
		m.status.InstallError = fmt.Sprintf("The update to Faro %s didn't finish. Try again, or download it from the release page.", target)
	}
}

// describeError turns network failures into something a person can act on.
func describeError(err error) string {
	var urlErr *url.Error
	var netErr net.Error
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "GitHub took too long to answer. Try again later."
	case errors.As(err, &dnsErr), errors.As(err, &netErr), errors.As(err, &urlErr):
		return "Couldn't reach GitHub. Check your internet connection and try again."
	}
	message := err.Error()
	if message == "" {
		return "Something went wrong."
	}
	return capitalise(message)
}

// capitalise makes an error read as a sentence.
func capitalise(message string) string {
	if message[0] >= 'a' && message[0] <= 'z' {
		message = string(message[0]-'a'+'A') + message[1:]
	}
	if !strings.HasSuffix(message, ".") && !strings.HasSuffix(message, "!") && !strings.HasSuffix(message, "?") {
		message += "."
	}
	return message
}
