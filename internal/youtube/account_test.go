package youtube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInspectSharesOneRunAndSkipsSponsorBlock(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	var args []string
	resolver := &Resolver{
		executable: "yt-dlp", runtimeArg: []string{"--js-runtimes", "node:/usr/bin/node"},
		run: func(_ context.Context, _ string, given ...string) ([]byte, error) {
			calls.Add(1)
			args = given
			<-release
			return []byte(`{"title":"Video","duration":95,"formats":[{"url":"v","height":720,"vcodec":"vp9"}]}`), nil
		},
	}
	var wait sync.WaitGroup
	results := make([]Info, 3)
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			info, err := resolver.Inspect(context.Background(), "https://youtu.be/example")
			if err != nil {
				t.Error(err)
			}
			results[index] = info
		}()
	}
	// Every caller is waiting on the one run before it finishes.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wait.Wait()
	if _, err := resolver.Inspect(context.Background(), "https://youtu.be/example"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("yt-dlp ran %d times, want once", calls.Load())
	}
	for _, info := range results {
		if info.Title != "Video" || info.Duration != 95 || len(info.Qualities) != 1 {
			t.Fatalf("unexpected details: %#v", info)
		}
	}
	if slices.Contains(args, "--sponsorblock-mark") {
		t.Fatalf("Inspect asked SponsorBlock for segments: %v", args)
	}
}

func TestInspectRetriesAfterFailureAndCallerGivingUp(t *testing.T) {
	var calls atomic.Int32
	resolver := &Resolver{
		executable: "yt-dlp", runtimeArg: []string{"--js-runtimes", "node:/usr/bin/node"},
		run: func(context.Context, string, ...string) ([]byte, error) {
			if calls.Add(1) <= 2 {
				return nil, errors.New("ERROR: temporary failure")
			}
			return []byte(`{"title":"Video"}`), nil
		},
	}
	if _, err := resolver.Inspect(context.Background(), "https://youtu.be/example"); err == nil {
		t.Fatal("a failed run was reported as success")
	}
	info, err := resolver.Inspect(context.Background(), "https://youtu.be/example")
	if err != nil || info.Title != "Video" {
		t.Fatalf("a failure was cached: info=%#v err=%v", info, err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolver.Inspect(cancelled, "https://youtu.be/other"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller that gave up got %v", err)
	}
}

func TestResolveUsesSponsorBlockAndAccount(t *testing.T) {
	var args []string
	resolver := &Resolver{
		executable: "yt-dlp", runtimeArg: []string{"--js-runtimes", "node:/usr/bin/node"},
		account: Account{Browser: "firefox", Profile: "work"},
		run: func(_ context.Context, _ string, given ...string) ([]byte, error) {
			args = given
			return []byte(`{"title":"Video","url":"stream","vcodec":"h264","acodec":"aac"}`), nil
		},
	}
	if _, err := resolver.Resolve(context.Background(), "https://youtu.be/example", 0); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--sponsorblock-mark") {
		t.Fatalf("Resolve did not mark SponsorBlock segments: %v", args)
	}
	index := slices.Index(args, "--cookies-from-browser")
	if index < 0 || args[index+1] != "firefox:work" || slices.Index(args, "--") < index {
		t.Fatalf("the sign-in was not passed before the URL: %v", args)
	}
}

func TestAccountNormalize(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(file, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := []struct {
		in   Account
		args []string
	}{
		{Account{}, nil},
		{Account{Browser: " Firefox ", Profile: "  "}, []string{"--cookies-from-browser", "firefox"}},
		{Account{Browser: "chrome", Profile: "Profile 1"}, []string{"--cookies-from-browser", "chrome:Profile 1"}},
		{Account{CookiesFile: file, Profile: "ignored"}, []string{"--cookies", file}},
	}
	for _, test := range valid {
		account, err := test.in.Normalize()
		if err != nil {
			t.Fatalf("%#v: %v", test.in, err)
		}
		if !slices.Equal(account.args(), test.args) {
			t.Fatalf("%#v: args = %v, want %v", test.in, account.args(), test.args)
		}
	}
	invalid := []Account{
		{Browser: "netscape"},
		{Browser: "firefox", Profile: "default::container"},
		{Browser: "firefox", Profile: "line\nbreak"},
		{Browser: "firefox", CookiesFile: file},
		{CookiesFile: "cookies.txt"},
		{CookiesFile: filepath.Join(t.TempDir(), "missing.txt")},
		{CookiesFile: t.TempDir()},
	}
	if runtime.GOOS != "darwin" {
		invalid = append(invalid, Account{Browser: "safari"})
	}
	for _, account := range invalid {
		if _, err := account.Normalize(); err == nil {
			t.Fatalf("%#v was accepted", account)
		}
	}
}

func TestBotCheckPointsAtSignIn(t *testing.T) {
	botCheck := errors.New("ERROR: [youtube] abc: Sign in to confirm you’re not a bot. Use --cookies-from-browser or --cookies for the authentication")
	var calls int
	resolver := &Resolver{
		executable: "yt-dlp", runtimeArg: []string{"--js-runtimes", "node:/usr/bin/node"},
		run: func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return nil, botCheck
		},
	}
	_, err := resolver.Resolve(context.Background(), "https://youtu.be/example", 0)
	if err == nil || !strings.Contains(err.Error(), "Preferences → YouTube") || calls != 2 {
		t.Fatalf("signed out: err = %v after %d attempts", err, calls)
	}
	resolver.account = Account{Browser: "firefox"}
	resolver.forgetInspections()
	if _, err := resolver.Inspect(context.Background(), "https://youtu.be/example"); err == nil || !strings.Contains(err.Error(), "with the sign-in from Firefox") {
		t.Fatalf("signed in: err = %v", err)
	}

	calls = 0
	resolver.run = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("ERROR: could not find firefox cookies database in /home/user/.mozilla/firefox")
	}
	if _, err := resolver.Resolve(context.Background(), "https://youtu.be/example", 0); err == nil || !strings.Contains(err.Error(), "could not read the YouTube sign-in from Firefox") || calls != 1 {
		t.Fatalf("unreadable sign-in: err = %v after %d attempts", err, calls)
	}
}

func TestTitleUsesOEmbedOrKnownDetails(t *testing.T) {
	var requested atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requested.Store(request.URL.Query().Get("url"))
		if strings.Contains(request.URL.Query().Get("url"), "private") {
			http.Error(writer, "Unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte(`{"title":" Quick title ","author_name":"Someone"}`))
	}))
	defer server.Close()
	resolver := &Resolver{oembedURL: server.URL, httpClient: server.Client()}
	title, err := resolver.Title(context.Background(), "https://www.youtube.com/watch?v=abc&t=5")
	if err != nil || title != "Quick title" || requested.Load() != "https://www.youtube.com/watch?v=abc&t=5" {
		t.Fatalf("title = %q, err = %v, requested %v", title, err, requested.Load())
	}
	if _, err := resolver.Title(context.Background(), "https://www.youtube.com/watch?v=private"); err == nil {
		t.Fatal("a refused lookup returned a title")
	}
	if _, err := resolver.Title(context.Background(), "https://example.com/video"); err == nil {
		t.Fatal("a non-YouTube link was looked up")
	}

	resolver.executable, resolver.runtimeArg = "yt-dlp", []string{"--js-runtimes", "node:/usr/bin/node"}
	resolver.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"title":"Full title","duration":10}`), nil
	}
	if _, err := resolver.Inspect(context.Background(), "https://youtu.be/known"); err != nil {
		t.Fatal(err)
	}
	if title, err := resolver.Title(context.Background(), "https://youtu.be/known"); err != nil || title != "Full title" {
		t.Fatalf("known details were not used: %q, %v", title, err)
	}
}

func TestReadAccountCheck(t *testing.T) {
	firefox := Account{Browser: "firefox"}
	signedIn := readAccountCheck(strings.NewReader(strings.Join([]string{
		"[debug] Command-line config: ['--verbose']",
		"Extracting cookies from firefox",
		"Extracted 812 cookies from firefox",
		"[debug] [youtube] Found YouTube account cookies",
		"[youtube] Extracting URL: https://www.youtube.com/watch?v=jNQXAC9IVRw",
		"[youtube] jNQXAC9IVRw: Downloading webpage",
	}, "\n")), firefox).status(firefox)
	if !signedIn.SignedIn || !strings.Contains(signedIn.Message, "Firefox") {
		t.Fatalf("signed in: %#v", signedIn)
	}
	signedOut := readAccountCheck(strings.NewReader("Extracted 3 cookies from firefox\nWARNING: cannot decrypt v11 cookies: no key found\n[youtube] Extracting URL: x\n"), firefox).status(firefox)
	if signedOut.SignedIn || !strings.Contains(signedOut.Message, "No YouTube sign-in found in Firefox") || !strings.Contains(signedOut.Message, "no key found") {
		t.Fatalf("signed out: %#v", signedOut)
	}
	failed := readAccountCheck(strings.NewReader("ERROR: could not find firefox cookies database in /home/user\n"), firefox).status(firefox)
	if failed.SignedIn || !strings.Contains(failed.Message, "could not find firefox cookies database") {
		t.Fatalf("unreadable: %#v", failed)
	}
}

func TestCheckAccountStopsBeforeContactingYouTube(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "contacted")
	script := filepath.Join(dir, "yt-dlp")
	// Diagnostics on stderr, progress on stdout, as yt-dlp writes them; the
	// script would go on to "contact YouTube" if it were not stopped.
	body := "#!/bin/sh\necho '[debug] [youtube] Found YouTube account cookies' >&2\necho '[youtube] Extracting URL: x'\nsleep 2\ntouch " + marker + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	resolver := &Resolver{find: func(name string) string {
		if name == "yt-dlp" {
			return script
		}
		return ""
	}}
	if status, err := resolver.CheckAccount(context.Background()); err != nil || status.SignedIn {
		t.Fatalf("without an account: %#v, %v", status, err)
	}
	resolver.account = Account{Browser: "firefox"}
	started := time.Now()
	status, err := resolver.CheckAccount(context.Background())
	if err != nil || !status.SignedIn {
		t.Fatalf("status = %#v, err = %v", status, err)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("the check waited %s for yt-dlp instead of stopping it", elapsed)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("yt-dlp was left running past the start of extraction")
	}
}
