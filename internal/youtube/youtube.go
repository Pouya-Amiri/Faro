package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sponsorCategories = "sponsor,intro,outro,selfpromo,interaction"

type Quality struct {
	Height int  `json:"height"`
	FPS    int  `json:"fps,omitempty"`
	HDR    bool `json:"hdr,omitempty"`
}

type Segment struct {
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	Category string  `json:"category,omitempty"`
	Title    string  `json:"title"`
}

type Info struct {
	Title    string  `json:"title"`
	Duration float64 `json:"duration"`
	// Live is set for a broadcast that is live now. Its duration, if any,
	// is the length of the window the provider serves, not of the video.
	Live      bool      `json:"live,omitempty"`
	Thumbnail string    `json:"thumbnail,omitempty"`
	Qualities []Quality `json:"qualities"`
	Segments  []Segment `json:"sponsorBlockSegments"`
	Backend   string    `json:"backend"`
}

type Stream struct {
	Info        Info
	VideoURL    string
	AudioURL    string
	CombinedURL string
}

type commandRunner func(context.Context, string, ...string) ([]byte, error)

type Resolver struct {
	mu         sync.Mutex
	executable string
	runtimeArg []string
	runtime    string
	// runtimeProblem explains why no JavaScript runtime is usable.
	runtimeProblem error
	run            commandRunner
	find           func(string) string
	version        func(context.Context, string) (int, int, bool)
}

// Support reports whether YouTube links can play. Problem says what is
// missing and is empty when they can.
type Support struct {
	YtDlp   string `json:"ytDlp,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	Problem string `json:"problem,omitempty"`
}

var (
	errYtDlpMissing   = errors.New("YouTube links need yt-dlp, which is not installed; install the current official yt-dlp build")
	errRuntimeMissing = errors.New("YouTube links need Deno 2.3+ or Node.js 22+ for yt-dlp, and neither is installed")
)

// jsRuntimes are the runtimes yt-dlp's EJS challenge solver supports, in
// order of preference. A zero minimum is not checked.
var jsRuntimes = []struct {
	name, executable, display string
	major, minor              int
}{
	{"deno", "deno", "Deno", 2, 3},
	{"node", "node", "Node.js", 22, 0},
	{"quickjs", "qjs", "QuickJS", 0, 0},
	{"bun", "bun", "Bun", 0, 0},
}

func NewResolver() *Resolver {
	return &Resolver{run: runCommand, find: findExecutable, version: runtimeVersion}
}

// tools returns yt-dlp and its runtime argument. Missing tools are looked for
// again on every use, so installing one takes effect without restarting Faro.
func (r *Resolver) tools(ctx context.Context) (string, []string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detect(ctx)
	if r.executable == "" {
		return "", nil, errYtDlpMissing
	}
	if len(r.runtimeArg) == 0 {
		if r.runtimeProblem != nil {
			return "", nil, r.runtimeProblem
		}
		return "", nil, errRuntimeMissing
	}
	return r.executable, r.runtimeArg, nil
}

func (r *Resolver) detect(ctx context.Context) {
	if r.find == nil {
		return
	}
	if r.executable == "" {
		r.executable = r.find("yt-dlp")
	}
	if len(r.runtimeArg) != 0 {
		return
	}
	r.runtimeProblem = errRuntimeMissing
	for _, candidate := range jsRuntimes {
		path := r.find(candidate.executable)
		if path == "" {
			continue
		}
		if candidate.major > 0 && r.version != nil {
			// An unreadable version is given the benefit of the doubt.
			if major, minor, ok := r.version(ctx, path); ok && (major < candidate.major || major == candidate.major && minor < candidate.minor) {
				r.runtimeProblem = fmt.Errorf("YouTube links need Deno 2.3+ or Node.js 22+ for yt-dlp; %s %d.%d is too old", candidate.display, major, minor)
				continue
			}
		}
		r.runtimeArg = []string{"--js-runtimes", candidate.name + ":" + path}
		r.runtime = candidate.display
		return
	}
}

// Support looks for the tools again if any is missing.
func (r *Resolver) Support(ctx context.Context) Support {
	_, _, err := r.tools(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	support := Support{YtDlp: r.executable, Runtime: r.runtime}
	if err != nil {
		support.Problem = err.Error()
	}
	return support
}

var versionPattern = regexp.MustCompile(`(\d+)\.(\d+)`)

// runtimeVersion reads the major and minor version from "node --version"
// ("v22.3.0") or "deno --version" ("deno 2.3.1 (stable, ...)").
func runtimeVersion(ctx context.Context, executable string) (int, int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := newCommandContext(ctx, executable, "--version").Output()
	if err != nil {
		return 0, 0, false
	}
	match := versionPattern.FindStringSubmatch(string(output))
	if match == nil {
		return 0, 0, false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	return major, minor, true
}

// Finder-launched macOS apps inherit a minimal PATH which commonly omits
// Homebrew, MacPorts, Deno, and Bun install directories. Keep dependencies
// external, but discover them where their normal installers place them.
func findExecutable(name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	if runtime.GOOS != "darwin" {
		return ""
	}
	home, _ := os.UserHomeDir()
	for _, directory := range darwinExecutableDirectories(home) {
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

func darwinExecutableDirectories(home string) []string {
	return []string{
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/opt/local/bin",
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".deno", "bin"),
		filepath.Join(home, ".bun", "bin"),
	}
}

func IsURL(source string) bool {
	parsed, err := url.Parse(strings.TrimSpace(source))
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com",
		"youtu.be", "youtube-nocookie.com", "www.youtube-nocookie.com":
		return true
	}
	return false
}

func (r *Resolver) Inspect(ctx context.Context, source string) (Info, error) {
	metadata, _, err := r.extract(ctx, source, 0, false)
	if err != nil {
		return Info{}, err
	}
	return infoFrom(metadata, r.executable), nil
}

func (r *Resolver) Resolve(ctx context.Context, source string, height int) (Stream, error) {
	metadata, _, err := r.extract(ctx, source, height, true)
	if err != nil {
		return Stream{}, err
	}
	stream := streamFrom(metadata, height)
	stream.Info = infoFrom(metadata, r.executable)
	if stream.VideoURL == "" && stream.CombinedURL == "" {
		return Stream{}, errors.New("yt-dlp returned no playable video stream")
	}
	return stream, nil
}

// ResolveFallback mirrors Faro's proven recovery path: the web_safari client
// and a single combined format avoid adaptive-stream requests that YouTube may
// selectively reject after metadata extraction has already succeeded.
func (r *Resolver) ResolveFallback(ctx context.Context, source string, height int) (Stream, error) {
	metadata, _, err := r.extractAttempts(ctx, source, height, true, []extractAttempt{{client: "youtube:player_client=web_safari", remote: true, combined: true}})
	if err != nil {
		return Stream{}, err
	}
	stream := streamFrom(metadata, height)
	stream.Info = infoFrom(metadata, r.executable)
	if stream.CombinedURL == "" {
		return Stream{}, errors.New("yt-dlp web_safari fallback returned no combined video stream")
	}
	return stream, nil
}

type extractAttempt struct {
	client   string
	remote   bool
	combined bool
}

func (r *Resolver) extract(ctx context.Context, source string, height int, resolve bool) (extractOutput, string, error) {
	attempts := []extractAttempt{
		{client: "youtube:player_client=web_embedded", remote: true},
		{client: "youtube:player_client=web_safari", remote: true, combined: true},
	}
	return r.extractAttempts(ctx, source, height, resolve, attempts)
}

func (r *Resolver) extractAttempts(ctx context.Context, source string, height int, resolve bool, attempts []extractAttempt) (extractOutput, string, error) {
	executable, runtimeArg, err := r.tools(ctx)
	if err != nil {
		return extractOutput{}, "", err
	}
	var failures []string
	for _, current := range attempts {
		args := []string{
			"--ignore-config", "--dump-single-json", "--skip-download", "--no-playlist",
			"--no-warnings", "--no-progress", "--no-colors",
			"--extractor-retries", "3", "--retries", "3", "--fragment-retries", "3",
			"--sponsorblock-mark", sponsorCategories,
		}
		args = append(args, runtimeArg...)
		if current.remote {
			args = append(args, "--remote-components", "ejs:github")
		}
		if current.client != "" {
			args = append(args, "--extractor-args", current.client)
		}
		if resolve {
			selector := formatSelector(height)
			if current.combined {
				selector = fallbackFormatSelector(height)
			}
			args = append(args, "--check-formats", "--format", selector)
			if height > 0 {
				args = append(args, "--format-sort", "res:"+strconv.Itoa(height)+",fps,hdr:12,codec")
			}
		}
		args = append(args, "--", source)
		raw, err := r.run(ctx, executable, args...)
		if err == nil {
			var metadata extractOutput
			if json.Unmarshal(raw, &metadata) == nil && metadata.Title != "" {
				return metadata, current.client, nil
			}
			err = errors.New("yt-dlp returned invalid metadata")
		}
		if rateLimited(err) {
			// Another client would only be another request YouTube refuses.
			return extractOutput{}, "", errRateLimited
		}
		failures = append(failures, conciseError(err))
	}
	return extractOutput{}, "", fmt.Errorf("YouTube extraction failed after all client fallbacks: %s", strings.Join(unique(failures), "; "))
}

func formatSelector(height int) string {
	if height <= 0 {
		return "bv*+ba/b"
	}
	limit := strconv.Itoa(height)
	return "bv*[height<=" + limit + "]+ba/b[height<=" + limit + "]"
}

func fallbackFormatSelector(height int) string {
	if height <= 0 {
		return "best"
	}
	return "best[height<=" + strconv.Itoa(height) + "]"
}

// runCommand returns stdout only: yt-dlp and the JavaScript runtime it
// starts print notices on stderr, which would corrupt the JSON on stdout.
// Stderr becomes the error message when the command fails.
func runCommand(ctx context.Context, executable string, args ...string) ([]byte, error) {
	output, err := newCommandContext(ctx, executable, args...).Output()
	if err != nil {
		var stderr []byte
		if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
			stderr = exitErr.Stderr
		}
		message := strings.TrimSpace(string(stderr))
		if len(message) > 1200 {
			message = message[len(message)-1200:]
		}
		if message != "" {
			return nil, errors.New(message)
		}
	}
	return output, err
}

type extractFormat struct {
	URL     string         `json:"url"`
	Height  float64        `json:"height"`
	FPS     float64        `json:"fps"`
	VCodec  string         `json:"vcodec"`
	ACodec  string         `json:"acodec"`
	Dynamic string         `json:"dynamic_range"`
	HasDRM  bool           `json:"has_drm"`
	Headers map[string]any `json:"http_headers"`
}

type extractChapter struct {
	Start    float64 `json:"start_time"`
	End      float64 `json:"end_time"`
	Title    string  `json:"title"`
	Category string  `json:"category"`
}

type extractOutput struct {
	Title            string           `json:"title"`
	IsLive           bool             `json:"is_live"`
	LiveStatus       string           `json:"live_status"`
	Duration         float64          `json:"duration"`
	Thumbnail        string           `json:"thumbnail"`
	URL              string           `json:"url"`
	VCodec           string           `json:"vcodec"`
	ACodec           string           `json:"acodec"`
	Formats          []extractFormat  `json:"formats"`
	RequestedFormats []extractFormat  `json:"requested_formats"`
	SponsorChapters  []extractChapter `json:"sponsorblock_chapters"`
}

func infoFrom(metadata extractOutput, executable string) Info {
	qualityByHeight := make(map[int]Quality)
	for _, format := range metadata.Formats {
		if format.HasDRM || format.URL == "" || format.VCodec == "" || format.VCodec == "none" || format.Height <= 0 {
			continue
		}
		height := int(format.Height)
		quality := qualityByHeight[height]
		quality.Height = height
		quality.FPS = max(quality.FPS, int(format.FPS+0.5))
		quality.HDR = quality.HDR || (format.Dynamic != "" && format.Dynamic != "SDR")
		qualityByHeight[height] = quality
	}
	qualities := make([]Quality, 0, len(qualityByHeight))
	for _, quality := range qualityByHeight {
		qualities = append(qualities, quality)
	}
	sort.Slice(qualities, func(i, j int) bool { return qualities[i].Height > qualities[j].Height })
	segments := make([]Segment, 0, len(metadata.SponsorChapters))
	for _, chapter := range metadata.SponsorChapters {
		if chapter.End <= chapter.Start {
			continue
		}
		segments = append(segments, Segment{Start: chapter.Start, End: chapter.End, Category: chapter.Category, Title: firstNonEmpty(chapter.Title, "SponsorBlock segment")})
	}
	live := metadata.LiveStatus == "is_live" || metadata.LiveStatus == "" && metadata.IsLive
	return Info{Title: metadata.Title, Duration: metadata.Duration, Live: live, Thumbnail: metadata.Thumbnail, Qualities: qualities, Segments: segments, Backend: executable}
}

func streamFrom(metadata extractOutput, height int) Stream {
	var result Stream
	for _, format := range metadata.RequestedFormats {
		if format.HasDRM || format.URL == "" {
			continue
		}
		hasVideo := format.VCodec != "" && format.VCodec != "none"
		hasAudio := format.ACodec != "" && format.ACodec != "none"
		switch {
		case hasVideo && hasAudio:
			result.CombinedURL = format.URL
		case hasVideo:
			result.VideoURL = format.URL
		case hasAudio:
			result.AudioURL = format.URL
		}
	}
	if result.VideoURL == "" && result.CombinedURL == "" && metadata.URL != "" {
		if metadata.VCodec != "" && metadata.VCodec != "none" && metadata.ACodec != "" && metadata.ACodec != "none" {
			result.CombinedURL = metadata.URL
		} else {
			result.VideoURL = metadata.URL
		}
	}
	var fallbackURL string
	var fallbackHeight float64 = -1
	var fallbackFPS float64 = -1
	for _, format := range metadata.Formats {
		if format.HasDRM || format.URL == "" || format.VCodec == "" || format.VCodec == "none" || format.ACodec == "" || format.ACodec == "none" {
			continue
		}
		if height > 0 && format.Height > float64(height) {
			continue
		}
		if format.Height > fallbackHeight || (format.Height == fallbackHeight && format.FPS > fallbackFPS) {
			fallbackURL, fallbackHeight, fallbackFPS = format.URL, format.Height, format.FPS
		}
	}
	if result.CombinedURL == "" {
		result.CombinedURL = fallbackURL
	}
	return result
}

var errRateLimited = errors.New("YouTube is limiting requests from this network (HTTP 429); try again in a few minutes")

// rateLimited reports whether yt-dlp failed because YouTube answered HTTP
// 429 Too Many Requests.
func rateLimited(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "HTTP Error 429") || strings.Contains(message, "Too Many Requests")
}

func conciseError(err error) string {
	if err == nil {
		return "unknown error"
	}
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func unique(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists || value == "" {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
