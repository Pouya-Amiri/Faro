package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Video details change rarely, but a live link (a channel's /live address)
// can point at another broadcast later, so its details expire sooner.
const (
	inspectionLifetime     = 10 * time.Minute
	liveInspectionLifetime = time.Minute
	// inspectTimeout bounds one shared yt-dlp run, which outlives the
	// caller that started it when others wait for it too.
	inspectTimeout   = 2 * time.Minute
	maxInspections   = 256
	defaultOEmbedURL = "https://www.youtube.com/oembed"
)

// inspection is one yt-dlp run for a video's details, shared by everyone who
// asks for the same video while it runs and for a while after it succeeded.
type inspection struct {
	done    chan struct{}
	info    Info
	err     error
	expires time.Time
}

type inspections struct {
	mu      sync.Mutex
	entries map[string]*inspection
}

// Inspect returns a video's title, duration and qualities. Concurrent
// requests for one video share a single yt-dlp run, and its result is reused
// for a while, so adding a link and showing its qualities ask YouTube once.
func (r *Resolver) Inspect(ctx context.Context, source string) (Info, error) {
	key := strings.TrimSpace(source)
	r.cache.mu.Lock()
	if r.cache.entries == nil {
		r.cache.entries = make(map[string]*inspection)
	}
	entry := r.cache.entries[key]
	if entry != nil && !entry.expires.IsZero() && time.Now().After(entry.expires) {
		entry = nil
	}
	if entry == nil {
		r.pruneLocked()
		entry = &inspection{done: make(chan struct{})}
		r.cache.entries[key] = entry
		// The run is shared, so one caller giving up must not end it for the
		// others; it ends with the run's own timeout.
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inspectTimeout)
		go func() {
			defer cancel()
			entry.info, entry.err = r.inspect(runCtx, key)
			r.cache.mu.Lock()
			if entry.err != nil {
				if r.cache.entries[key] == entry {
					delete(r.cache.entries, key)
				}
			} else if entry.info.Live {
				entry.expires = time.Now().Add(liveInspectionLifetime)
			} else {
				entry.expires = time.Now().Add(inspectionLifetime)
			}
			r.cache.mu.Unlock()
			close(entry.done)
		}()
	}
	r.cache.mu.Unlock()
	select {
	case <-entry.done:
		return entry.info, entry.err
	case <-ctx.Done():
		return Info{}, ctx.Err()
	}
}

// pruneLocked drops expired details and, past the limit, the oldest ones.
func (r *Resolver) pruneLocked() {
	now := time.Now()
	for key, entry := range r.cache.entries {
		if !entry.expires.IsZero() && now.After(entry.expires) {
			delete(r.cache.entries, key)
		}
	}
	for len(r.cache.entries) >= maxInspections {
		var oldestKey string
		var oldest time.Time
		for key, entry := range r.cache.entries {
			if !entry.expires.IsZero() && (oldestKey == "" || entry.expires.Before(oldest)) {
				oldestKey, oldest = key, entry.expires
			}
		}
		if oldestKey == "" {
			return // every entry is still running
		}
		delete(r.cache.entries, oldestKey)
	}
}

// forgetInspections drops finished details, for example after the sign-in
// changed. Runs in progress finish for those already waiting on them.
func (r *Resolver) forgetInspections() {
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	for key, entry := range r.cache.entries {
		if !entry.expires.IsZero() {
			delete(r.cache.entries, key)
		}
	}
}

// cachedInfo returns details already known for source.
func (r *Resolver) cachedInfo(source string) (Info, bool) {
	r.cache.mu.Lock()
	entry := r.cache.entries[strings.TrimSpace(source)]
	r.cache.mu.Unlock()
	if entry == nil {
		return Info{}, false
	}
	select {
	case <-entry.done:
		return entry.info, entry.err == nil
	default:
		return Info{}, false
	}
}

func (r *Resolver) inspect(ctx context.Context, source string) (Info, error) {
	// SponsorBlock segments are only needed for playback, which resolves the
	// video again; leaving them out saves a request to the SponsorBlock API.
	metadata, _, err := r.extractAttempts(ctx, source, 0, false, false, defaultAttempts)
	if err != nil {
		return Info{}, err
	}
	return infoFrom(metadata, r.ytDlpPath()), nil
}

// Title returns a video's title quickly from YouTube's oEmbed endpoint, the
// one embeds use, without starting yt-dlp. It fails for private, removed and
// non-video links (a channel's /live address), which need Inspect instead.
func (r *Resolver) Title(ctx context.Context, source string) (string, error) {
	if !IsURL(source) {
		return "", errors.New("a YouTube URL is required")
	}
	if info, ok := r.cachedInfo(source); ok && info.Title != "" {
		return info.Title, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := r.oembedURL
	if endpoint == "" {
		endpoint = defaultOEmbedURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+url.Values{"format": {"json"}, "url": {strings.TrimSpace(source)}}.Encode(), nil)
	if err != nil {
		return "", err
	}
	client := r.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("YouTube oEmbed returned %s", response.Status)
	}
	var embed struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&embed); err != nil {
		return "", err
	}
	title := strings.TrimSpace(embed.Title)
	if title == "" {
		return "", errors.New("YouTube oEmbed returned no title")
	}
	return title, nil
}
