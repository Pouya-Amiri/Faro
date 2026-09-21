package app

import (
	"os"
	"path/filepath"
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
