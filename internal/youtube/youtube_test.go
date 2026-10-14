package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestInfoFromMetadataBuildsQualitiesAndSponsorSegments(t *testing.T) {
	metadata := extractOutput{
		Title: "Video", Duration: 120,
		Formats: []extractFormat{
			{URL: "video-1080", Height: 1080, FPS: 60, VCodec: "vp9", ACodec: "none", Dynamic: "HDR10"},
			{URL: "video-1080-30", Height: 1080, FPS: 30, VCodec: "h264", ACodec: "none"},
			{URL: "video-720", Height: 720, FPS: 30, VCodec: "h264", ACodec: "none"},
			{URL: "drm", Height: 2160, VCodec: "vp9", HasDRM: true},
		},
		SponsorChapters: []extractChapter{{Start: 10, End: 20, Title: "Sponsor", Category: "sponsor"}},
	}
	info := infoFrom(metadata, "yt-dlp")
	if len(info.Qualities) != 2 || info.Qualities[0].Height != 1080 || info.Qualities[0].FPS != 60 || !info.Qualities[0].HDR {
		t.Fatalf("unexpected qualities: %#v", info.Qualities)
	}
	if len(info.Segments) != 1 || info.Segments[0].End != 20 {
		t.Fatalf("unexpected SponsorBlock segments: %#v", info.Segments)
	}
}

func TestStreamFromAdaptiveMetadata(t *testing.T) {
	metadata := extractOutput{
		RequestedFormats: []extractFormat{
			{URL: "video", VCodec: "vp9", ACodec: "none"},
			{URL: "audio", VCodec: "none", ACodec: "opus"},
		},
		Formats: []extractFormat{
			{URL: "combined-360", Height: 360, VCodec: "h264", ACodec: "aac"},
			{URL: "combined-720", Height: 720, VCodec: "h264", ACodec: "aac"},
		},
	}
	stream := streamFrom(metadata, 0)
	if stream.VideoURL != "video" || stream.AudioURL != "audio" || stream.CombinedURL != "combined-720" {
		raw, _ := json.Marshal(stream)
		t.Fatalf("unexpected stream: %s", raw)
	}
}

func TestFormatSelectorUsesQualityCapWithFallback(t *testing.T) {
	if got := formatSelector(1080); got != "bv*[height<=1080]+ba/b[height<=1080]" {
		t.Fatalf("unexpected selector: %s", got)
	}
}

func TestYouTubeURLRecognition(t *testing.T) {
	for _, source := range []string{
		"https://www.youtube.com/watch?v=x", "https://youtu.be/x", "https://m.youtube.com/watch?v=x",
		"https://music.youtube.com/watch?v=x", "https://www.youtube-nocookie.com/embed/x", "HTTPS://WWW.YOUTUBE.COM/watch?v=x",
	} {
		if !IsURL(source) {
			t.Fatalf("expected YouTube URL: %s", source)
		}
	}
	for _, source := range []string{"https://example.com/watch?v=x", "https://youtube.com.evil.example/watch?v=x", "ftp://youtube.com/x", "youtube.com/watch?v=x"} {
		if IsURL(source) {
			t.Fatalf("non-YouTube URL was accepted: %s", source)
		}
	}
}

func TestDarwinExecutableDirectoriesCoverCommonInstallers(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "faro")
	got := darwinExecutableDirectories(home)
	for _, want := range []string{
		"/opt/homebrew/bin",
		"/usr/local/bin",
		filepath.Join(home, ".deno", "bin"),
		filepath.Join(home, ".bun", "bin"),
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("missing macOS executable directory %q in %v", want, got)
		}
	}
}

func TestResolverRetriesWithAlternateYouTubeClient(t *testing.T) {
	var calls [][]string
	resolver := &Resolver{
		executable: "yt-dlp", runtimeArg: []string{"--js-runtimes", "node:/usr/bin/node"},
		run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			if len(calls) < 2 {
				return nil, errors.New("blocked")
			}
			return []byte(`{"title":"Video","url":"stream","vcodec":"h264","acodec":"aac"}`), nil
		},
	}
	stream, err := resolver.Resolve(context.Background(), "https://youtu.be/example", 1080)
	if err != nil {
		t.Fatal(err)
	}
	if stream.CombinedURL != "stream" || len(calls) != 2 {
		t.Fatalf("unexpected fallback result: stream=%#v calls=%d", stream, len(calls))
	}
	if !slices.Contains(calls[0], "ejs:github") || !slices.Contains(calls[0], "--check-formats") {
		t.Fatalf("primary attempt did not enable remote EJS and format checks: %#v", calls[0])
	}
	if !slices.Contains(calls[0], "youtube:player_client=web_embedded") {
		t.Fatalf("primary YouTube client was not attempted: %#v", calls[0])
	}
	if !slices.Contains(calls[1], "youtube:player_client=web_safari") || !slices.Contains(calls[1], fallbackFormatSelector(1080)) {
		t.Fatalf("alternate YouTube client was not attempted: %#v", calls[1])
	}
}

func TestFallbackFormatUsesCombinedVideo(t *testing.T) {
	if got := fallbackFormatSelector(1080); got != "best[height<=1080]" {
		t.Fatalf("unexpected fallback selector: %s", got)
	}
}

func TestRunCommandParsesStdoutOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	script := filepath.Join(t.TempDir(), "fake-yt-dlp")
	body := "#!/bin/sh\necho 'Downloading deno runtime...' >&2\necho '{\"title\":\"x\"}'\n[ \"$1\" = fail ] && { echo 'ERROR: video unavailable' >&2; exit 1; }\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := runCommand(context.Background(), script)
	if err != nil || strings.TrimSpace(string(output)) != `{"title":"x"}` {
		t.Fatalf("output = %q, err = %v", output, err)
	}
	if _, err := runCommand(context.Background(), script, "fail"); err == nil || !strings.Contains(err.Error(), "video unavailable") {
		t.Fatalf("failure error = %v, want the stderr message", err)
	}
}

func TestResolverSkipsRuntimesTooOldForYtDlp(t *testing.T) {
	installed := map[string]string{"yt-dlp": "/bin/yt-dlp", "deno": "/bin/deno", "node": "/bin/node"}
	versions := map[string][2]int{"/bin/deno": {2, 1}, "/bin/node": {22, 3}}
	resolver := &Resolver{
		find: func(name string) string { return installed[name] },
		version: func(_ context.Context, path string) (int, int, bool) {
			version := versions[path]
			return version[0], version[1], true
		},
	}
	support := resolver.Support(context.Background())
	if support.Problem != "" || support.Runtime != "Node.js" || !slices.Equal(resolver.runtimeArg, []string{"--js-runtimes", "node:/bin/node"}) {
		t.Fatalf("support = %#v, runtime argument = %v", support, resolver.runtimeArg)
	}

	versions["/bin/node"] = [2]int{20, 11}
	resolver = &Resolver{find: resolver.find, version: resolver.version}
	if support := resolver.Support(context.Background()); !strings.Contains(support.Problem, "Node.js 20.11 is too old") {
		t.Fatalf("old runtimes were accepted: %#v", support)
	}
}

func TestResolverFindsToolsInstalledAfterStart(t *testing.T) {
	installed := map[string]string{}
	resolver := &Resolver{find: func(name string) string { return installed[name] }}
	if support := resolver.Support(context.Background()); !strings.Contains(support.Problem, "yt-dlp, which is not installed") {
		t.Fatalf("missing yt-dlp was not reported: %#v", support)
	}
	installed["yt-dlp"] = "/bin/yt-dlp"
	if support := resolver.Support(context.Background()); !strings.Contains(support.Problem, "neither is installed") {
		t.Fatalf("missing runtime was not reported: %#v", support)
	}
	installed["qjs"] = "/bin/qjs"
	if support := resolver.Support(context.Background()); support.Problem != "" || support.YtDlp != "/bin/yt-dlp" || support.Runtime != "QuickJS" {
		t.Fatalf("installed tools were not found: %#v", support)
	}
}
