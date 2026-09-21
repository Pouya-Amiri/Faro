package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
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
	Title     string    `json:"title"`
	Duration  float64   `json:"duration"`
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
	executable string
	runtimeArg []string
	run        commandRunner
}

func NewResolver() *Resolver {
	executable := findExecutable("yt-dlp")
	var runtimeArg []string
	for _, candidate := range []struct{ name, executable string }{
		{"deno", "deno"}, {"node", "node"}, {"quickjs", "qjs"}, {"bun", "bun"},
	} {
		if path := findExecutable(candidate.executable); path != "" {
			runtimeArg = []string{"--js-runtimes", candidate.name + ":" + path}
			break
		}
	}
	return &Resolver{executable: executable, runtimeArg: runtimeArg, run: runCommand}
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
	lower := strings.ToLower(strings.TrimSpace(source))
	return strings.HasPrefix(lower, "https://youtube.com/") ||
		strings.HasPrefix(lower, "https://www.youtube.com/") ||
		strings.HasPrefix(lower, "https://m.youtube.com/") ||
		strings.HasPrefix(lower, "https://youtu.be/") ||
		strings.HasPrefix(lower, "http://youtube.com/") ||
		strings.HasPrefix(lower, "http://www.youtube.com/") ||
		strings.HasPrefix(lower, "http://youtu.be/")
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
	if r.executable == "" {
		return extractOutput{}, "", errors.New("yt-dlp was not found; install the current official yt-dlp build")
	}
	if len(r.runtimeArg) == 0 {
		return extractOutput{}, "", errors.New("YouTube playback requires Deno 2.3+, Node.js 22+, or another yt-dlp EJS runtime")
	}
	var failures []string
	for _, current := range attempts {
		args := []string{
			"--ignore-config", "--dump-single-json", "--skip-download", "--no-playlist",
			"--no-warnings", "--no-progress", "--no-colors",
			"--extractor-retries", "3", "--retries", "3", "--fragment-retries", "3",
			"--sponsorblock-mark", sponsorCategories,
		}
		args = append(args, r.runtimeArg...)
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
		raw, err := r.run(ctx, r.executable, args...)
		if err == nil {
			var metadata extractOutput
			if json.Unmarshal(raw, &metadata) == nil && metadata.Title != "" {
				return metadata, current.client, nil
			}
			err = errors.New("yt-dlp returned invalid metadata")
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

func runCommand(ctx context.Context, executable string, args ...string) ([]byte, error) {
	output, err := newCommandContext(ctx, executable, args...).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
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
	return Info{Title: metadata.Title, Duration: metadata.Duration, Thumbnail: metadata.Thumbnail, Qualities: qualities, Segments: segments, Backend: executable}
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
