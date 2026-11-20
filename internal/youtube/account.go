package youtube

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
)

// Account is the YouTube sign-in yt-dlp uses. Faro never handles a password:
// the user signs in to YouTube in a browser, and yt-dlp reads that browser's
// YouTube cookies on this device. The zero value uses no account.
type Account struct {
	// Browser is the browser whose sign-in yt-dlp uses, such as "firefox".
	Browser string `json:"browser,omitempty"`
	// Profile optionally names the browser profile, by name or path.
	Profile string `json:"profile,omitempty"`
	// CookiesFile is a Netscape cookies.txt file used instead of a browser.
	CookiesFile string `json:"cookiesFile,omitempty"`
}

// browsers are the browsers yt-dlp can read cookies from, with the names
// Faro shows for them.
var browsers = map[string]string{
	"firefox": "Firefox", "chrome": "Chrome", "chromium": "Chromium", "brave": "Brave",
	"edge": "Edge", "opera": "Opera", "vivaldi": "Vivaldi", "whale": "Whale", "safari": "Safari",
}

// maxCookiesFile bounds a cookies.txt file; real ones are a few kilobytes.
const maxCookiesFile = 8 << 20

// Normalize validates an account and returns it in canonical form.
func (a Account) Normalize() (Account, error) {
	a.Browser = strings.ToLower(strings.TrimSpace(a.Browser))
	a.Profile = strings.TrimSpace(a.Profile)
	a.CookiesFile = strings.TrimSpace(a.CookiesFile)
	switch {
	case a.Browser != "" && a.CookiesFile != "":
		return Account{}, errors.New("choose either a browser or a cookies file for YouTube, not both")
	case a.Browser != "":
		if _, ok := browsers[a.Browser]; !ok {
			return Account{}, fmt.Errorf("yt-dlp cannot read YouTube sign-ins from %q", a.Browser)
		}
		if a.Browser == "safari" && runtime.GOOS != "darwin" {
			return Account{}, errors.New("Safari sign-ins can only be used on macOS")
		}
		// yt-dlp reads BROWSER[:PROFILE][::CONTAINER]; a profile must not
		// turn into a container or carry anything that is not a name or path.
		if len(a.Profile) > 512 || strings.Contains(a.Profile, "::") || strings.IndexFunc(a.Profile, unicode.IsControl) >= 0 {
			return Account{}, errors.New("the browser profile must be a profile name or folder")
		}
	case a.CookiesFile != "":
		a.Profile = ""
		if !filepath.IsAbs(a.CookiesFile) {
			return Account{}, errors.New("the cookies file must be given by its full path")
		}
		a.CookiesFile = filepath.Clean(a.CookiesFile)
		info, err := os.Stat(a.CookiesFile)
		if err != nil {
			return Account{}, fmt.Errorf("cannot use the cookies file: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() > maxCookiesFile {
			return Account{}, errors.New("the cookies file must be a cookies.txt file")
		}
	default:
		a.Profile = ""
	}
	return a, nil
}

// Enabled reports whether an account is configured.
func (a Account) Enabled() bool { return a.Browser != "" || a.CookiesFile != "" }

// source names where the sign-in comes from, for messages.
func (a Account) source() string {
	if a.CookiesFile != "" {
		return "the cookies file"
	}
	return browsers[a.Browser]
}

// args are the yt-dlp options that use the account.
func (a Account) args() []string {
	switch {
	case a.Browser != "":
		spec := a.Browser
		if a.Profile != "" {
			spec += ":" + a.Profile
		}
		return []string{"--cookies-from-browser", spec}
	case a.CookiesFile != "":
		return []string{"--cookies", a.CookiesFile}
	}
	return nil
}

// SetAccount changes the sign-in used from the next yt-dlp run on. Cached
// video details are dropped, since a signed-in session can see more.
func (r *Resolver) SetAccount(account Account) error {
	account, err := account.Normalize()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.account = account
	r.mu.Unlock()
	r.forgetInspections()
	return nil
}

// Account returns the sign-in yt-dlp uses.
func (r *Resolver) Account() Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.account
}

func (r *Resolver) accountArgs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.account.args()
}

// AccountStatus is the result of checking a YouTube sign-in.
type AccountStatus struct {
	SignedIn bool   `json:"signedIn"`
	Message  string `json:"message"`
}

// accountCheckURL is any video URL: the check stops yt-dlp before it asks
// YouTube for anything.
const accountCheckURL = "https://www.youtube.com/watch?v=jNQXAC9IVRw"

// CheckAccount reports whether yt-dlp can read a YouTube sign-in from the
// configured browser or file. yt-dlp loads the cookies and looks for
// YouTube's sign-in cookies before it extracts anything, so it is stopped as
// soon as extraction would begin and nothing is sent to YouTube.
func (r *Resolver) CheckAccount(ctx context.Context) (AccountStatus, error) {
	r.mu.Lock()
	r.detect(ctx)
	executable, account := r.executable, r.account
	r.mu.Unlock()
	if executable == "" {
		return AccountStatus{}, errYtDlpMissing
	}
	if !account.Enabled() {
		return AccountStatus{Message: "No account is used. YouTube links play signed out."}, nil
	}
	return checkAccount(ctx, executable, account)
}

func checkAccount(ctx context.Context, executable string, account Account) (AccountStatus, error) {
	// Reading a browser's cookies can wait on a keyring or keychain prompt.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := append([]string{"--verbose", "--ignore-config", "--simulate", "--no-playlist", "--no-progress", "--no-colors"}, account.args()...)
	command := newCommandContext(ctx, executable, append(args, "--", accountCheckURL)...)
	command.WaitDelay = time.Second
	// Progress goes to stdout and diagnostics to stderr; one pipe keeps
	// them in order.
	command.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	output, input, err := os.Pipe()
	if err != nil {
		return AccountStatus{}, err
	}
	defer output.Close()
	command.Stdout, command.Stderr = input, input
	err = command.Start()
	input.Close()
	if err != nil {
		return AccountStatus{}, err
	}
	result := readAccountCheck(output, account)
	// Everything needed has been read; yt-dlp must not go on to YouTube.
	_ = command.Process.Kill()
	_ = command.Wait()
	if ctx.Err() != nil && !result.decided {
		return AccountStatus{}, errors.New("checking the YouTube sign-in took too long")
	}
	return result.status(account), nil
}

type accountCheck struct {
	decided  bool
	found    bool
	problem  string // why the cookies could not be read
	warnings []string
}

// readAccountCheck reads yt-dlp's verbose output up to the start of
// extraction.
func readAccountCheck(output io.Reader, account Account) accountCheck {
	var check accountCheck
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.Contains(line, "Found YouTube account cookies"):
			check.found = true
		case strings.HasPrefix(line, "ERROR:"):
			check.problem = strings.TrimSpace(strings.TrimPrefix(line, "ERROR:"))
			check.decided = true
			return check
		case strings.HasPrefix(line, "WARNING:") && strings.Contains(strings.ToLower(line), "cookie"):
			check.warnings = append(check.warnings, strings.TrimSpace(strings.TrimPrefix(line, "WARNING:")))
		case strings.Contains(line, "Extracting URL:"):
			check.decided = true
			return check
		}
	}
	return check
}

func (c accountCheck) status(account Account) AccountStatus {
	source := account.source()
	switch {
	case c.problem != "":
		message := fmt.Sprintf("Faro could not read the YouTube sign-in from %s: %s", source, c.problem)
		if runtime.GOOS == "windows" && account.Browser != "" && account.Browser != "firefox" {
			message += ". On Windows, Chromium-based browsers keep their sign-in from other programs; use Firefox instead"
		}
		return AccountStatus{Message: message}
	case c.found:
		return AccountStatus{SignedIn: true, Message: fmt.Sprintf("Signed in to YouTube with %s.", source)}
	case !c.decided:
		return AccountStatus{Message: fmt.Sprintf("Faro could not check the YouTube sign-in from %s.", source)}
	}
	message := fmt.Sprintf("No YouTube sign-in found in %s. Sign in to YouTube there, then check again.", source)
	if account.CookiesFile != "" {
		message = "The cookies file has no YouTube sign-in. Export it again while signed in to YouTube."
	}
	if len(c.warnings) != 0 {
		message += " yt-dlp reported: " + c.warnings[0]
	}
	return AccountStatus{Message: message}
}

// botCheck reports whether YouTube refused a video until the viewer signs in
// ("Sign in to confirm you're not a bot").
func botCheck(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "not a bot") || strings.Contains(message, "sign in to confirm")
}

// cookieProblem reports whether yt-dlp stopped because it could not read the
// configured sign-in. Trying another YouTube client would not help.
func cookieProblem(message string) bool {
	message = strings.ToLower(message)
	for _, marker := range []string{"failed to load cookies", "cookies database", "could not copy", "failed to decrypt", "cookie file", "keyring"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// signInError explains a failure YouTube's bot check or an unreadable
// sign-in caused, pointing at the YouTube preferences.
func signInError(account Account, failures []string) error {
	for _, failure := range failures {
		if account.Enabled() && cookieProblem(failure) {
			return fmt.Errorf("yt-dlp could not read the YouTube sign-in from %s (%s); check it in Preferences → YouTube", account.source(), failure)
		}
	}
	for _, failure := range failures {
		if !botCheck(failure) {
			continue
		}
		if account.Enabled() {
			return fmt.Errorf("YouTube asks this device to confirm it is not a bot even with the sign-in from %s; sign in to YouTube there again, then check it in Preferences → YouTube", account.source())
		}
		return errors.New("YouTube asks this device to confirm it is not a bot; in Preferences → YouTube, choose a browser in which you are signed in to YouTube")
	}
	return nil
}
