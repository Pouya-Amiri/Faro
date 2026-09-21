package vlc

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchArgumentsTargetTheLuaCliInterface(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		args := launchArguments(goos, "127.0.0.1:1234", nil, "movie.mkv")
		if !containsArgument(args, "--extraintf=luaintf") || !containsArgument(args, "--lua-intf=cli") {
			t.Fatalf("%s arguments do not target the Lua cli interface: %q", goos, args)
		}
		if !containsArgument(args, "--cli-host=127.0.0.1:1234") || containsArgument(args, "--rc-host=127.0.0.1:1234") {
			t.Fatalf("%s arguments do not configure the Lua cli socket: %q", goos, args)
		}
		if containsArgument(args, "--rc-quiet") {
			t.Fatalf("%s arguments contain --rc-quiet: %q", goos, args)
		}
	}
	macArgs := launchArguments("darwin", "127.0.0.1:1234", nil, "")
	for _, unsupported := range []string{"--no-one-instance", "--no-one-instance-when-started-from-file"} {
		if containsArgument(macArgs, unsupported) {
			t.Fatalf("macOS arguments contain unsupported instance option %q: %q", unsupported, macArgs)
		}
	}
	for _, goos := range []string{"linux", "windows"} {
		args := launchArguments(goos, "127.0.0.1:1234", nil, "")
		if !containsArgument(args, "--no-one-instance") || !containsArgument(args, "--no-one-instance-when-started-from-file") {
			t.Fatalf("%s arguments do not isolate Faro's VLC process: %q", goos, args)
		}
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		args := launchArguments(goos, "127.0.0.1:1234", []string{"--rc-quiet", "--no-audio"}, "movie.mkv")
		if containsArgument(args, "--rc-quiet") {
			t.Fatalf("%s arguments kept a configured --rc-quiet: %q", goos, args)
		}
		if !containsArgument(args, "--no-audio") || args[len(args)-1] != "movie.mkv" {
			t.Fatalf("%s arguments lost configured values: %q", goos, args)
		}
	}
}

func TestLaunchArgumentsRejectControlChannelOverrides(t *testing.T) {
	args := launchArguments("windows", "127.0.0.1:1234", []string{
		"--lua-intf=dummy", "--cli-host", "127.0.0.1:9999", "--extraintf=http",
		"-I", "dummy", "-Ihttp", "--one-instance", "--no-audio",
	}, "")
	for _, forbidden := range []string{
		"--lua-intf=dummy", "127.0.0.1:9999", "--extraintf=http", "-I", "dummy", "-Ihttp", "--one-instance",
	} {
		if containsArgument(args, forbidden) {
			t.Fatalf("VLC arguments retained protected override %q: %q", forbidden, args)
		}
	}
	for _, required := range []string{
		"--lua-intf=cli", "--cli-host=127.0.0.1:1234", "--extraintf=luaintf", "--no-one-instance", "--no-audio",
	} {
		if !containsArgument(args, required) {
			t.Fatalf("VLC arguments dropped required option %q: %q", required, args)
		}
	}
}

func TestParsePlaybackStateDistinguishesPausedFromPlaying(t *testing.T) {
	for name, response := range map[string]string{
		"playing": "status change: ( audio volume: 1.000000 )\n( state playing )",
		"paused":  "( state paused )",
		"stopped": "( state stopped )",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parsePlaybackState(response)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]playbackState{"playing": playbackPlaying, "paused": playbackPaused, "stopped": playbackStopped}[name]
			if got != want {
				t.Fatalf("parsePlaybackState() = %v, want %v", got, want)
			}
		})
	}
	if _, err := parsePlaybackState("status change: ( stop state: 0 )"); err == nil {
		t.Fatal("parsePlaybackState accepted a response without a playback state")
	}
}

func TestSeekArgumentUsesVLCWholeSecondSyntax(t *testing.T) {
	for input, want := range map[float64]string{-1: "0", 1.25: "1", 1.75: "2"} {
		got, err := seekArgument(input)
		if err != nil || got != want {
			t.Fatalf("seekArgument(%v) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestResponseErrorRejectsVLCFailures(t *testing.T) {
	for _, response := range []string{
		"Unknown command `rate'. Type `help' for help.",
		"seek: returned -1 (generic error)",
		"Error in `add file:///missing.mkv' command",
	} {
		if err := responseError("test", response); err == nil {
			t.Fatalf("responseError accepted %q", response)
		}
	}
	if err := responseError("test", "rate: returned 0 (no error)"); err != nil {
		t.Fatalf("responseError rejected success: %v", err)
	}
}

func TestParseNumberResponseIgnoresNumbersInsideStatusLines(t *testing.T) {
	got, err := parseNumberResponse("status change: ( audio volume: 38.0 )\n12")
	if err != nil || got != 12 {
		t.Fatalf("parseNumberResponse() = %v, %v; want 12", got, err)
	}
}

func TestHasRCCommandParsesHelpTable(t *testing.T) {
	help := "+----[ Remote control commands ]\n| rate [playback rate]\n| seek X"
	if !hasRCCommand(help, "rate") || hasRCCommand(help, "chapter") {
		t.Fatalf("hasRCCommand parsed help incorrectly")
	}
}

func containsArgument(arguments []string, target string) bool {
	for _, argument := range arguments {
		if argument == target {
			return true
		}
	}
	return false
}

func TestVLCMRLPercentEncodesLocalPathsWithoutQuotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "A movie #1.mkv")
	mrl, err := vlcMRL(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(mrl, "file:///") || strings.ContainsAny(mrl, ` "`) {
		t.Fatalf("vlcMRL(%q) returned unsafe MRL %q", path, mrl)
	}
	if !strings.Contains(mrl, "A%20movie%20%231.mkv") {
		t.Fatalf("VLC MRL was not percent encoded: %q", mrl)
	}
}

func TestVLCMRLPreservesStreamURLs(t *testing.T) {
	const source = "https://media.example/movie.mp4?token=one%20two&part=1"
	mrl, err := vlcMRL(source)
	if err != nil || mrl != source {
		t.Fatalf("vlcMRL(%q) = %q, %v", source, mrl, err)
	}
}

func TestVLCMRLRejectsRCCommandInjection(t *testing.T) {
	if _, err := vlcMRL("movie.mkv\nquit"); err == nil {
		t.Fatal("VLC MRL accepted a line break")
	}
}

func TestValidateLocalSourceRejectsMissingFilesButAllowsURLs(t *testing.T) {
	if err := validateLocalSource(filepath.Join(t.TempDir(), "missing.mkv")); err == nil {
		t.Fatal("missing local file was accepted")
	}
	if err := validateLocalSource("https://media.example/movie.mkv"); err != nil {
		t.Fatalf("remote URL was rejected: %v", err)
	}
}

func TestLocalVLCMRLHandlesWindowsDriveAndUNCPaths(t *testing.T) {
	if got := localVLCMRL(`C:\Users\Ada\My Movie.mkv`, "windows"); got != "file:///C:/Users/Ada/My%20Movie.mkv" {
		t.Fatalf("Windows drive MRL = %q", got)
	}
	if got := localVLCMRL(`\\server\media\My Movie.mkv`, "windows"); got != "file://server/media/My%20Movie.mkv" {
		t.Fatalf("Windows UNC MRL = %q", got)
	}
}
