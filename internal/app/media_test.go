package app

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestExpandMediaPathsIncludesFilesAndDirectories(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "season")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(root, "episode-1.mkv")
	audio := filepath.Join(nested, "episode-2.flac")
	ignored := filepath.Join(root, "notes.txt")
	for _, path := range []string{video, audio, ignored} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files, err := ExpandMediaPaths([]string{video, nested, ignored}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != video || files[1] != audio {
		t.Fatalf("unexpected expanded media: %#v", files)
	}
}

func TestExpandMediaPathsEnforcesLimit(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one.mkv", "two.mp4"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ExpandMediaPaths([]string{root}, 1); err == nil {
		t.Fatal("expected playlist item limit error")
	}
}

func TestMediaFilesInDirectorySkipsUnreadableFolders(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	for _, path := range []string{filepath.Join(root, "a.mkv"), filepath.Join(locked, "b.mkv"), filepath.Join(root, "z", "c.mp4")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	files, err := MediaFilesInDirectory(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(root, "a.mkv"), filepath.Join(root, "z", "c.mp4")}; !slices.Equal(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	if _, err := MediaFilesInDirectory(filepath.Join(root, "missing"), 10); err == nil {
		t.Fatal("a missing folder was not reported")
	}
}
