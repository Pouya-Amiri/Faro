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
	for _, player := range []string{"mpv", "mpv.net", "IINA", "Memento", "VLC"} {
		if _, names, err := executableNames(player); err != nil || len(names) == 0 {
			t.Fatalf("executableNames(%q) = %v, %v", player, names, err)
		}
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
