package discovery

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestFindUsesSelectedPlayersPathLauncher(t *testing.T) {
	directory := t.TempDir()
	name := "mpv"
	if runtime.GOOS == "windows" {
		name = "mpv.exe"
	}
	executable := filepath.Join(directory, name)
	if err := os.WriteFile(executable, []byte("test launcher"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)

	found, err := Find("mpv")
	if err != nil {
		t.Fatal(err)
	}
	if found != executable {
		t.Fatalf("Find returned %q, want %q", found, executable)
	}
}

func TestExecutableNamesAcceptFrontendPlayerValues(t *testing.T) {
	supported := SupportedPlayers()
	for _, player := range supported {
		if _, names, err := executableNames(player); err != nil || len(names) == 0 {
			t.Fatalf("executableNames(%q) = %v, %v", player, names, err)
		}
	}
	for _, player := range []string{"mpv", "mpv.net", "iina", "memento", "vlc"} {
		if slices.Contains(supported, player) {
			continue
		}
		if _, _, err := executableNames(player); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("executableNames(%q) should reject this platform, got %v", player, err)
		}
	}
}

func TestSupportedPlayersMatchPlatform(t *testing.T) {
	want := map[string][]string{
		"darwin":  {"mpv", "iina", "memento", "vlc"},
		"windows": {"mpv", "mpv.net", "memento", "vlc"},
		"linux":   {"mpv", "memento", "vlc"},
	}[runtime.GOOS]
	if want != nil && !slices.Equal(SupportedPlayers(), want) {
		t.Fatalf("SupportedPlayers() = %v, want %v", SupportedPlayers(), want)
	}
}

func TestPreferWindowsExecutablesAvoidsConsoleStubs(t *testing.T) {
	got := preferWindowsExecutables([]string{"mpv", "mpv.com", "mpv.exe"})
	want := []string{"mpv.exe", "mpv.com", "mpv"}
	if !slices.Equal(got, want) {
		t.Fatalf("preferWindowsExecutables() = %v, want %v", got, want)
	}
}

func TestFindRejectsUnsupportedPlayer(t *testing.T) {
	_, err := Find("unknown")
	if err == nil || !strings.Contains(err.Error(), "unsupported player") {
		t.Fatalf("Find returned %v", err)
	}
}

func TestExpandCandidatesExpandsAndDeduplicatesGlobs(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "VLC.AppImage")
	if err := os.WriteFile(first, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	got := expandCandidates([]string{filepath.Join(directory, "*.AppImage"), first})
	if len(got) != 1 || got[0] != first {
		t.Fatalf("expandCandidates returned %v", got)
	}
}

func TestUsableFileRejectsNonExecutableFilesOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows launchability is based on executable extension")
	}
	path := filepath.Join(t.TempDir(), "mpv")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if usableFile(path) {
		t.Fatal("non-executable file was accepted as a player launcher")
	}
}
