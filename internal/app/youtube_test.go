package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

// detailsResolver reports a video's details after a delay, as yt-dlp does.
type detailsResolver struct {
	release  chan struct{}
	inspects atomic.Int32
}

func (r *detailsResolver) Inspect(ctx context.Context, _ string) (youtube.Info, error) {
	r.inspects.Add(1)
	select {
	case <-r.release:
	case <-ctx.Done():
		return youtube.Info{}, ctx.Err()
	}
	return youtube.Info{Title: "Full title", Duration: 212}, nil
}

func (r *detailsResolver) Resolve(context.Context, string, int) (youtube.Stream, error) {
	return youtube.Stream{CombinedURL: "https://cdn.example.test/video.mp4"}, nil
}

func TestQueuedYouTubeLinkGetsItsDurationLater(t *testing.T) {
	s, _ := liveRoom(t, nil)
	resolver := &detailsResolver{release: make(chan struct{})}
	s.youtube = resolver
	const link = "https://www.youtube.com/watch?v=abcdefghijk"
	if _, err := s.AppendPlaylist([]PlaylistInput{{Label: "Quick title", Source: link}}, false); err != nil {
		t.Fatal(err)
	}
	client, err := s.connected()
	if err != nil {
		t.Fatal(err)
	}
	items := client.Snapshot().Playlist.Items
	if len(items) != 1 || items[0].Label != "Quick title" || items[0].Media == nil || items[0].Media.DurationSeconds != 0 {
		t.Fatalf("the link was not queued at once: %#v", items)
	}
	close(resolver.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		items = client.Snapshot().Playlist.Items
		if len(items) == 1 && items[0].Media != nil && items[0].Media.DurationSeconds == 212 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the duration was never filled in: %#v", items[0].Media)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if items[0].Label != "Quick title" || items[0].URL != link {
		t.Fatalf("filling in the duration changed the item: %#v", items[0])
	}

	// A link queued with its duration is not looked up again.
	before := resolver.inspects.Load()
	if _, err := s.AppendPlaylist([]PlaylistInput{{Label: "Known", Source: link + "2", DurationSeconds: 30}}, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if resolver.inspects.Load() != before {
		t.Fatal("a link with a known duration was inspected again")
	}
}
