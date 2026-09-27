package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Pouya-Amiri/Faro/internal/player"
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

func TestAppendPlaylistKeepsOtherEditsAndSkipsMissingFiles(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	var events []Event
	var eventsMu sync.Mutex
	service := New(context.Background(), func(event Event) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	})
	status, err := service.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Shutdown)
	mediaPlayer := newLifecyclePlayer()
	service.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return mediaPlayer, nil }
	if err := service.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	write := func(name string) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	existing, added := write("existing.mkv"), write("added.mkv")
	if err := service.SetPlaylist([]PlaylistInput{{Label: "Existing", Source: existing}}); err != nil {
		t.Fatal(err)
	}
	count, err := service.AppendPlaylist([]PlaylistInput{
		{Label: "Missing", Source: filepath.Join(directory, "missing.mkv")},
		{Label: "Added", Source: added},
	}, true)
	if err != nil || count != 1 {
		t.Fatalf("AppendPlaylist() = %d, %v; want 1 item added", count, err)
	}
	playlist := service.client.Snapshot().Playlist
	if len(playlist.Items) != 2 || playlist.Items[0].Label != "Existing" || playlist.Items[1].Label != "Added" || playlist.Selected != 1 {
		t.Fatalf("playlist = %#v, want the existing item kept and the added one selected", playlist)
	}
	eventsMu.Lock()
	reported := slices.ContainsFunc(events, func(event Event) bool {
		return event.Error != nil && event.Error.Code == "playlist_skipped" && strings.Contains(event.Error.Message, "missing.mkv")
	})
	eventsMu.Unlock()
	if !reported {
		t.Fatal("the missing file was not reported")
	}
	if _, err := service.AppendPlaylist([]PlaylistInput{{Label: "Gone", Source: filepath.Join(directory, "gone.mkv")}}, false); err == nil {
		t.Fatal("a batch with nothing readable must fail")
	}
}
