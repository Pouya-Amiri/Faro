package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub serves a latest-release document, its assets and checksums.
type fakeGitHub struct {
	server   *httptest.Server
	tag      string
	files    map[string][]byte
	sums     string
	requests atomic.Int32
	// truncateOnce cuts the first download of each file short.
	truncateOnce bool
	truncated    sync.Map
}

func newFakeGitHub(t *testing.T, tag string, files map[string][]byte) *fakeGitHub {
	t.Helper()
	fake := &fakeGitHub{tag: tag, files: files}
	var sums strings.Builder
	for name, data := range files {
		digest := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(digest[:]), name)
	}
	fake.sums = sums.String()
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeGitHub) assetPrefix() string { return f.server.URL + "/download/" }

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	switch {
	case r.URL.Path == "/latest":
		type item struct {
			Name  string `json:"name"`
			Size  int    `json:"size"`
			State string `json:"state"`
			URL   string `json:"browser_download_url"`
		}
		assets := []item{{Name: checksumsName, Size: len(f.sums), State: "uploaded", URL: f.assetPrefix() + checksumsName}}
		for name, data := range f.files {
			assets = append(assets, item{Name: name, Size: len(data), State: "uploaded", URL: f.assetPrefix() + name})
		}
		// An asset hosted elsewhere must be ignored.
		assets = append(assets, item{Name: "evil.bin", Size: 1, URL: "https://example.invalid/evil.bin"})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     f.tag,
			"body":         "## What's Changed\n* Faster sync",
			"html_url":     releasePageURL + "tag/" + f.tag,
			"published_at": "2026-09-28T17:20:44Z",
			"assets":       assets,
		})
	case r.URL.Path == "/download/"+checksumsName:
		_, _ = w.Write([]byte(f.sums))
	case strings.HasPrefix(r.URL.Path, "/download/"):
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		data, ok := f.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		start := 0
		if header := r.Header.Get("Range"); header != "" {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(header, "bytes="), "-"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		}
		if _, seen := f.truncated.LoadOrStore(name, true); f.truncateOnce && !seen {
			// Send half, then drop the connection.
			_, _ = w.Write(data[start : start+(len(data)-start)/2])
			if hijacker, ok := w.(http.Hijacker); ok {
				connection, _, _ := hijacker.Hijack()
				_ = connection.Close()
			}
			return
		}
		_, _ = w.Write(data[start:])
	default:
		http.NotFound(w, r)
	}
}

func testManager(t *testing.T, fake *fakeGitHub, current string, detected installation, apply applyFunc) (*Manager, chan Status) {
	t.Helper()
	updates := make(chan Status, 1024)
	manager, err := New(Options{
		CurrentVersion: current,
		Notify: func(status Status) {
			select {
			case updates <- status:
			default:
			}
		},
		client:         fake.server.Client(),
		downloadClient: fake.server.Client(),
		endpoint:       fake.server.URL + "/latest",
		assetPrefix:    fake.assetPrefix(),
		cacheDir:       t.TempDir(),
		detect:         func() installation { return detected },
		apply:          apply,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager, updates
}

func waitForStatus(t *testing.T, updates chan Status, accept func(Status) bool) Status {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case status := <-updates:
			if accept(status) {
				return status
			}
		case <-deadline:
			t.Fatal("timed out waiting for the expected update status")
		}
	}
}

func appImageInstallation() installation {
	return installation{method: MethodAppImage, target: "/tmp/Faro.AppImage"}
}

func TestCheckReportsNewerRelease(t *testing.T) {
	current := appImageInstallation()
	name := current.assetName("1.2.0")
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{name: []byte("new faro")})
	manager, _ := testManager(t, fake, "1.1.1", current, nil)
	status, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || status.LatestVersion != "1.2.0" || status.CurrentVersion != "1.1.1" {
		t.Fatalf("newer release was not reported: %#v", status)
	}
	if !status.CanInstall || status.Method != MethodAppImage || status.DownloadSize != int64(len("new faro")) {
		t.Fatalf("installable release was not recognised: %#v", status)
	}
	if status.ReleaseURL != releasePageURL+"tag/v1.2.0" || !strings.Contains(status.ReleaseNotes, "Faster sync") || status.PublishedAt == 0 {
		t.Fatalf("release details are missing: %#v", status)
	}
	if status.Phase != PhaseIdle || status.CheckedAt == 0 || status.CheckError != "" {
		t.Fatalf("check did not settle: %#v", status)
	}
}

func TestCheckIgnoresSameOrOlderRelease(t *testing.T) {
	for _, tag := range []string{"v1.0.3", "v1.0.2", "v1.0.3-rc.1"} {
		t.Run(tag, func(t *testing.T) {
			fake := newFakeGitHub(t, tag, map[string][]byte{})
			manager, _ := testManager(t, fake, "1.0.3", appImageInstallation(), nil)
			status, err := manager.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status.Available || status.CanInstall {
				t.Fatalf("release %s was incorrectly offered over 1.0.3: %#v", tag, status)
			}
		})
	}
}

func TestCheckExplainsManualInstallations(t *testing.T) {
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{})
	manual := installation{method: MethodManual, reason: "Not packaged."}
	manager, _ := testManager(t, fake, "1.1.1", manual, nil)
	status, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || status.CanInstall || status.Note != "Not packaged." {
		t.Fatalf("manual installation was offered an automatic update: %#v", status)
	}
	if err := manager.Install(); err == nil {
		t.Fatal("Install accepted an update that must be installed by hand")
	}
}

func TestCheckRequiresTheMatchingAsset(t *testing.T) {
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{"Faro-1.2.0-something-else.zip": []byte("x")})
	manager, _ := testManager(t, fake, "1.1.1", appImageInstallation(), nil)
	status, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || status.CanInstall || status.Note == "" {
		t.Fatalf("release without this platform's file was installable: %#v", status)
	}
}

func TestCheckRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name, body string
		code       int
	}{
		{name: "rate limited", code: http.StatusForbidden, body: `{}`},
		{name: "server error", code: http.StatusBadGateway, body: `{}`},
		{name: "invalid json", code: http.StatusOK, body: `{`},
		{name: "invalid tag", code: http.StatusOK, body: `{"tag_name":"latest"}`},
		{name: "draft", code: http.StatusOK, body: `{"tag_name":"v9.0.0","draft":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.code)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			manager, err := New(Options{CurrentVersion: "1.0.3", client: server.Client(), endpoint: server.URL, cacheDir: t.TempDir(), detect: appImageInstallation})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			status, err := manager.Check(context.Background())
			if err == nil || status.CheckError == "" || status.Available {
				t.Fatalf("invalid release response was accepted: %v %#v", err, status)
			}
		})
	}
}

func TestInstallDownloadsVerifiesAppliesAndRestarts(t *testing.T) {
	current := appImageInstallation()
	name := current.assetName("1.2.0")
	payload := []byte(strings.Repeat("faro update ", 50_000))
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{name: payload})
	fake.truncateOnce = true

	var applied string
	relaunched := atomic.Bool{}
	apply := func(_ context.Context, got installation, file, release string) (func() error, error) {
		if got != current || release != "1.2.0" {
			t.Errorf("apply received %#v for %s", got, release)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		applied = string(data)
		return func() error { relaunched.Store(true); return nil }, nil
	}
	manager, _ := testManager(t, fake, "1.1.1", current, apply)
	restarted := make(chan struct{})
	manager.options.Restart = func() { close(restarted) }
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("Faro was not restarted after installing")
	}
	if applied != string(payload) || !relaunched.Load() {
		t.Fatalf("update was not applied intact (relaunched %v, %d bytes)", relaunched.Load(), len(applied))
	}
	// The interrupted first download was resumed rather than failing.
	if status := manager.Status(); !status.Downloaded || status.InstallError != "" {
		t.Fatalf("unexpected status after install: %#v", status)
	}

	// The next launch, now at 1.2.0, reports the completed update once.
	next, err := New(Options{CurrentVersion: "1.2.0", cacheDir: manager.options.cacheDir, detect: appImageInstallation})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if status := next.Status(); status.UpdatedFrom != "1.1.1" {
		t.Fatalf("completed update was not reported: %#v", status)
	}
	if _, err := os.Stat(next.pendingPath()); !os.IsNotExist(err) {
		t.Fatal("pending update marker was not cleared")
	}
}

func TestInstallRejectsTamperedDownload(t *testing.T) {
	current := appImageInstallation()
	name := current.assetName("1.2.0")
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{name: []byte("genuine")})
	fake.files[name] = []byte("tampered")
	applied := atomic.Bool{}
	manager, updates := testManager(t, fake, "1.1.1", current, func(context.Context, installation, string, string) (func() error, error) {
		applied.Store(true)
		return func() error { return nil }, nil
	})
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(); err != nil {
		t.Fatal(err)
	}
	status := waitForStatus(t, updates, func(status Status) bool { return status.InstallError != "" })
	if applied.Load() || status.Phase != PhaseIdle || !strings.Contains(status.InstallError, "checksum") {
		t.Fatalf("tampered update was not rejected: %#v", status)
	}
	if entries, _ := filepath.Glob(filepath.Join(manager.options.cacheDir, "1.2.0", "*")); len(entries) != 0 {
		t.Fatalf("rejected download was kept: %v", entries)
	}
}

func TestInstallCancelledByUserIsNotAnError(t *testing.T) {
	current := appImageInstallation()
	name := current.assetName("1.2.0")
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{name: []byte("new")})
	manager, updates := testManager(t, fake, "1.1.1", current, func(context.Context, installation, string, string) (func() error, error) {
		return nil, errCancelled
	})
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(); err != nil {
		t.Fatal(err)
	}
	status := waitForStatus(t, updates, func(status Status) bool { return status.Phase == PhaseIdle && status.Downloaded })
	if status.InstallError != "" || !status.Available || !status.Downloaded {
		t.Fatalf("declined authentication was reported as a failure: %#v", status)
	}
	if _, err := os.Stat(manager.pendingPath()); !os.IsNotExist(err) {
		t.Fatal("pending marker survived a failed install")
	}
}

func TestUnfinishedUpdateIsReportedUntilAnotherReleaseAppears(t *testing.T) {
	cache := t.TempDir()
	data, _ := json.Marshal(pendingUpdate{Version: "1.2.0", From: "1.1.1"})
	if err := os.WriteFile(filepath.Join(cache, "pending.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	current := appImageInstallation()
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{current.assetName("1.2.0"): []byte("x")})
	manager, err := New(Options{
		CurrentVersion: "1.1.1", client: fake.server.Client(), endpoint: fake.server.URL + "/latest",
		assetPrefix: fake.assetPrefix(), cacheDir: cache, detect: func() installation { return current },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	status, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.UpdatedFrom != "" || !strings.Contains(status.InstallError, "1.2.0") {
		t.Fatalf("unfinished update was not reported: %#v", status)
	}
}

func TestDownloadResumesPartialFile(t *testing.T) {
	payload := []byte(strings.Repeat("0123456789", 10_000))
	digest := sha256.Sum256(payload)
	var ranges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		start, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start:])
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "update.bin.part"), payload[:4000], 0o600); err != nil {
		t.Fatal(err)
	}
	item := asset{name: "update.bin", size: int64(len(payload)), url: server.URL + "/update.bin"}
	path, err := download(context.Background(), server.Client(), item, hex.EncodeToString(digest[:]), dir, "test", func(int64, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(payload) || len(ranges) != 1 || ranges[0] != "bytes=4000-" {
		t.Fatalf("download did not resume (ranges %v)", ranges)
	}
	// A verified file is reused without another request.
	if _, err := download(context.Background(), server.Client(), item, hex.EncodeToString(digest[:]), dir, "test", func(int64, int64) {}); err != nil || len(ranges) != 1 {
		t.Fatalf("verified download was fetched again: %v %v", err, ranges)
	}
}

func TestDownloadCanBeCancelled(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	go func() {
		<-started
		cancel()
	}()
	item := asset{name: "update.bin", size: 100, url: server.URL}
	_, err := download(ctx, server.Client(), item, strings.Repeat("0", 64), t.TempDir(), "test", func(received, _ int64) {
		if received > 0 {
			select {
			case started <- struct{}{}:
			default:
			}
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled download returned %v", err)
	}
}

func TestParseChecksums(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	sums := parseChecksums([]byte(hash + "  Faro.AppImage\n" + strings.ToUpper(hash) + " *Faro.exe\nnot a line\nzz  bad\n"))
	if sums["Faro.AppImage"] != hash || sums["Faro.exe"] != hash || len(sums) != 2 {
		t.Fatalf("unexpected checksums: %#v", sums)
	}
}

func TestAssetNamesMatchReleaseFiles(t *testing.T) {
	// These mirror the names the release workflow publishes (Taskfile.yml,
	// packaging/windows and packaging/macos).
	tests := []struct {
		method       Method
		amd64, arm64 string
	}{
		{MethodWindowsInstaller, "Faro-1.2.0-windows-x86_64-setup.exe", "Faro-1.2.0-windows-arm64-setup.exe"},
		{MethodWindowsPortable, "Faro-1.2.0-windows-x86_64-portable.zip", "Faro-1.2.0-windows-arm64-portable.zip"},
		{MethodMacApp, "Faro-1.2.0-macos-x86_64.dmg", "Faro-1.2.0-macos-arm64.dmg"},
		{MethodAppImage, "Faro-1.2.0-linux-x86_64.AppImage", "Faro-1.2.0-linux-aarch64.AppImage"},
		{MethodDeb, "Faro-1.2.0-linux-amd64.deb", "Faro-1.2.0-linux-arm64.deb"},
		{MethodRPM, "Faro-1.2.0-linux-x86_64.rpm", "Faro-1.2.0-linux-aarch64.rpm"},
		{MethodManual, "", ""},
	}
	for _, test := range tests {
		current := installation{method: test.method}
		if got := current.assetNameFor("1.2.0", "amd64"); got != test.amd64 {
			t.Errorf("%s amd64 asset = %q, want %q", test.method, got, test.amd64)
		}
		if got := current.assetNameFor("1.2.0", "arm64"); got != test.arm64 {
			t.Errorf("%s arm64 asset = %q, want %q", test.method, got, test.arm64)
		}
	}
}

func TestDescribeErrorReadsAsASentence(t *testing.T) {
	if got := describeError(errors.New("the release has no file")); got != "The release has no file." {
		t.Fatalf("describeError = %q", got)
	}
	if got := describeError(context.DeadlineExceeded); !strings.Contains(got, "too long") {
		t.Fatalf("describeError(timeout) = %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestCheckDuringInstallKeepsTheReleaseBeingInstalled(t *testing.T) {
	current := appImageInstallation()
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{current.assetName("1.2.0"): []byte("x")})
	applying, release := make(chan struct{}), make(chan struct{})
	manager, _ := testManager(t, fake, "1.1.1", current, func(context.Context, installation, string, string) (func() error, error) {
		close(applying)
		<-release
		return nil, errCancelled
	})
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A second check that answers 1.3.0 only after the install has begun.
	fake.tag = "v1.3.0"
	gate := make(chan struct{})
	transport := manager.options.client.Transport
	manager.options.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/latest" {
			<-gate
		}
		return transport.RoundTrip(request)
	})
	checked := make(chan struct{})
	go func() { _, _ = manager.Check(context.Background()); close(checked) }()
	for manager.Status().Phase != PhaseChecking {
		time.Sleep(5 * time.Millisecond)
	}
	if err := manager.Install(); err != nil {
		t.Fatal(err)
	}
	<-applying
	close(gate)
	<-checked
	if status := manager.Status(); status.LatestVersion != "1.2.0" || !status.Downloaded || status.Phase != PhaseInstalling {
		t.Fatalf("a check replaced the release being installed: %#v", status)
	}
	close(release)
}

func TestCancelledDownloadDoesNotReuseCache(t *testing.T) {
	dir := t.TempDir()
	data := []byte("cached")
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := download(ctx, http.DefaultClient, asset{name: "a.bin", size: 6, url: "http://127.0.0.1:1/"}, hex.EncodeToString(digest[:]), dir, "test", func(int64, int64) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled download returned %v", err)
	}
}

func TestCancelBeforeInstallingIsHonoured(t *testing.T) {
	current := appImageInstallation()
	name := current.assetName("1.2.0")
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{name: []byte("new")})
	applied := atomic.Bool{}
	manager, updates := testManager(t, fake, "1.1.1", current, func(context.Context, installation, string, string) (func() error, error) {
		applied.Store(true)
		return func() error { return nil }, nil
	})
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Cancel as the download starts; the install must never begin.
	for len(updates) > 0 {
		<-updates
	}
	manager.options.downloadClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := fake.server.Client().Transport.RoundTrip(request)
		if err == nil && strings.HasSuffix(request.URL.Path, name) {
			manager.Cancel()
		}
		return response, err
	})}
	if err := manager.Install(); err != nil {
		t.Fatal(err)
	}
	status := waitForStatus(t, updates, func(status Status) bool { return status.Phase == PhaseIdle })
	if applied.Load() || status.InstallError != "" {
		t.Fatalf("a cancelled update was installed: applied=%v %#v", applied.Load(), status)
	}
}

func TestBeforeRestartRunsBeforeRestart(t *testing.T) {
	current := appImageInstallation()
	fake := newFakeGitHub(t, "v1.2.0", map[string][]byte{current.assetName("1.2.0"): []byte("new")})
	manager, _ := testManager(t, fake, "1.1.1", current, func(context.Context, installation, string, string) (func() error, error) {
		return func() error { return nil }, nil
	})
	var order []string
	var mu sync.Mutex
	restarted := make(chan struct{})
	manager.options.BeforeRestart = func() { mu.Lock(); order = append(order, "prepare"); mu.Unlock() }
	manager.options.Restart = func() { mu.Lock(); order = append(order, "restart"); mu.Unlock(); close(restarted) }
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(); err != nil {
		t.Fatal(err)
	}
	<-restarted
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "prepare,restart" {
		t.Fatalf("restart hooks ran as %v", order)
	}
}

func TestBuildMetadataIsKeptInAssetNames(t *testing.T) {
	current := appImageInstallation()
	name := current.assetName("1.2.0+build.42")
	if name != "Faro-1.2.0+build.42-linux-"+archName(runtime.GOARCH, "x86_64", "aarch64")+".AppImage" {
		t.Fatalf("unexpected asset name %q", name)
	}
	fake := newFakeGitHub(t, "v1.2.0+build.42", map[string][]byte{name: []byte("x")})
	manager, _ := testManager(t, fake, "1.1.1", current, nil)
	status, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.CanInstall || status.LatestVersion != "1.2.0+build.42" {
		t.Fatalf("release with build metadata was not installable: %#v", status)
	}
}

func TestDownloadRetriesCleanShortBody(t *testing.T) {
	payload := []byte("abcdefgh")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			// Chunked and ended cleanly, but short.
			w.(http.Flusher).Flush()
			_, _ = w.Write(payload[:4])
			return
		}
		start, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start:])
	}))
	defer server.Close()
	digest := sha256.Sum256(payload)
	path, err := download(context.Background(), server.Client(), asset{name: "b.bin", size: 8, url: server.URL}, hex.EncodeToString(digest[:]), t.TempDir(), "test", func(int64, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(payload) || requests.Load() != 2 {
		t.Fatalf("short body was not resumed: %q after %d requests", got, requests.Load())
	}
}
