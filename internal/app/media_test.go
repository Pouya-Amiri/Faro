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
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
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
	if err := service.SetPlaylist([]PlaylistInput{{Label: "Existing", Source: existing}}, nil); err != nil {
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

func TestQueueEditsAreRebasedInsteadOfOverwritingOthers(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	host := New(context.Background(), nil)
	status, err := host.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Shutdown)
	guest := New(context.Background(), nil)
	t.Cleanup(guest.Shutdown)
	for _, service := range []*Service{host, guest} {
		service.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	}
	if err := host.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	if err := guest.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Guest", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	url := func(name string) PlaylistInput {
		return PlaylistInput{Label: name, URL: "https://example.com/" + name + ".mp4"}
	}
	if _, err := host.AppendPlaylist([]PlaylistInput{url("one"), url("two"), url("three")}, false); err != nil {
		t.Fatal(err)
	}
	labels := func(service *Service) []string {
		result := []string{}
		for _, item := range service.client.Snapshot().Playlist.Items {
			result = append(result, item.Label)
		}
		return result
	}
	waitForLabels := func(service *Service, want []string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !slices.Equal(labels(service), want) {
			if time.Now().After(deadline) {
				t.Fatalf("queue = %q, want %q", labels(service), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForLabels(guest, []string{"one", "two", "three"})

	// The guest's edit is computed from a queue the host changes before the
	// edit is published, so the server rejects it and it is redone.
	attempts := 0
	err = guest.editPlaylist(guest.client, func(items []protocol.PlaylistItem) ([]protocol.PlaylistItem, error) {
		attempts++
		if attempts == 1 {
			if _, err := host.AppendPlaylist([]PlaylistInput{url("four")}, false); err != nil {
				return nil, err
			}
		}
		return slices.DeleteFunc(items, func(item protocol.PlaylistItem) bool { return item.Label == "one" }), nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("edit returned %v after %d attempts, want success on the second", err, attempts)
	}
	waitForLabels(host, []string{"two", "three", "four"})

	// Removing and moving work on item IDs, wherever the items are now.
	items := host.client.Snapshot().Playlist.Items
	if err := guest.MovePlaylistItem(items[2].ID, items[0].ID); err != nil {
		t.Fatal(err)
	}
	waitForLabels(host, []string{"four", "two", "three"})
	if err := guest.RemovePlaylistItem(items[1].ID); err != nil {
		t.Fatal(err)
	}
	waitForLabels(host, []string{"four", "two"})
	if err := guest.RemovePlaylistItem(items[1].ID); err != nil {
		t.Fatalf("removing an item that is already gone returned %v", err)
	}
	if err := guest.SelectPlaylist(0, items[0].ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for host.client.Snapshot().Playlist.Selected != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("selected %d, want the item with ID %q at index 1", host.client.Snapshot().Playlist.Selected, items[0].ID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
