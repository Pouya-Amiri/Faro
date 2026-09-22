package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/mediaid"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

func (s *Service) applySelectedPlaylist(ctx context.Context, client *faroclient.Client) {
	s.mediaLoadMu.Lock()
	defer s.mediaLoadMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	s.mu.RLock()
	current := s.client == client
	s.mu.RUnlock()
	if !current {
		return
	}
	playlist := client.Snapshot().Playlist
	if playlist.Selected < 0 || playlist.Selected >= len(playlist.Items) {
		return
	}
	item := playlist.Items[playlist.Selected]
	identity := playlistItemIdentity(item)
	s.mu.Lock()
	if item.ID == s.selectedItem && identity == s.selectedItemIdentity {
		s.mu.Unlock()
		return
	}
	source := item.URL
	if source == "" && item.Media != nil {
		source = s.sources[item.Media.Fingerprint]
	}
	s.mu.Unlock()
	if source == "" {
		s.sink(Event{Kind: "error", Error: &protocol.Error{
			Code: "media_missing", Message: fmt.Sprintf("Locate a local copy of %q before playing it", item.Label),
		}})
		return
	}
	currentClient, mediaPlayer, err := s.ensurePlayer(playerStartAutomatic)
	if err != nil || currentClient != client {
		if err != nil && !errors.Is(err, errPlayerDismissed) {
			s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "open_player", Message: err.Error()}})
		}
		return
	}
	openCtx, cancel := s.mediaOperationContext(ctx)
	s.beginMediaTransition()
	info, err := s.openPlayerSource(openCtx, mediaPlayer, source)
	if err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "open_media", Message: err.Error()}})
		cancel()
		s.endMediaTransition()
		return
	}
	state := s.waitForPlayerMedia(openCtx, mediaPlayer, info.Duration)
	latest := client.Snapshot().Playlist
	if ctx.Err() != nil || latest.Selected < 0 || latest.Selected >= len(latest.Items) || latest.Items[latest.Selected].ID != item.ID || playlistItemIdentity(latest.Items[latest.Selected]) != identity {
		cancel()
		s.endMediaTransition()
		return
	}
	playback := localClockSnapshot(client).Playback
	_ = mediaPlayer.Seek(openCtx, playback.PositionSeconds)
	_ = mediaPlayer.SetRate(openCtx, normalizedRate(playback.Rate))
	media := item.Media
	if media == nil {
		media, _ = s.identifyAndRemember(source, item.Label, firstPositive(info.Duration, state.DurationSeconds))
	}
	if media != nil {
		copy := *media
		if info.Title != "" {
			copy.Title = info.Title
		}
		if info.Duration > 0 {
			copy.DurationSeconds = info.Duration
		} else if state.DurationSeconds > 0 {
			copy.DurationSeconds = state.DurationSeconds
		}
		_ = currentClient.SetMedia(protocol.MediaSet{Media: &copy})
	}
	s.mu.Lock()
	s.selectedItem = item.ID
	s.selectedItemIdentity = identity
	s.mu.Unlock()
	// Playback may have changed while loading (notably when Pause is pressed
	// just after a wheel spin). Re-read the authoritative state at the last
	// possible moment and finish atomically with queued local controls.
	playback = localClockSnapshot(client).Playback
	if _, err := s.finishMediaTransition(openCtx, mediaPlayer, playback.Paused, nil); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "player_sync", Message: err.Error()}})
	}
	cancel()
}

func (s *Service) SetPlaylist(inputs []PlaylistInput) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	items := make([]protocol.PlaylistItem, 0, len(inputs))
	for _, input := range inputs {
		item := protocol.PlaylistItem{ID: input.ID, Label: strings.TrimSpace(input.Label), URL: input.URL, Media: input.Media}
		if input.Source != "" || input.URL != "" {
			result, inspectErr := mediaid.Inspect(firstNonEmpty(input.Source, input.URL), item.Label, 0)
			if inspectErr != nil {
				return inspectErr
			}
			item.URL, item.Media = result.URL, &result.Media
			if result.Path != "" {
				s.rememberSource(result.Media.Fingerprint, result.Path)
			}
		}
		if item.Label == "" && item.Media != nil {
			item.Label = item.Media.Title
		}
		items = append(items, item)
	}
	if err := client.SetPlaylist(protocol.PlaylistSet{Items: items}); err != nil {
		return err
	}
	// The playlist update event performs the same reconciliation for every
	// participant; doing it here also closes a removed provider stream promptly.
	go s.reconcilePlaylistStreams(s.root, client)
	return nil
}

func (s *Service) SelectPlaylist(index int) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	return client.SelectPlaylist(index)
}

func (s *Service) SpinPlaylistWheel() error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	return client.SpinPlaylistWheel()
}

func (s *Service) OpenMedia(source string) error {
	s.mediaLoadMu.Lock()
	defer s.mediaLoadMu.Unlock()
	client, mediaPlayer, err := s.ensurePlayer(playerStartExplicit)
	if err != nil {
		return err
	}
	ctx, cancel := s.mediaOperationContext(s.root)
	defer cancel()
	identity, err := mediaid.Inspect(source, "", 0)
	if err != nil {
		return err
	}
	s.beginMediaTransition()
	finished := false
	defer func() {
		if !finished {
			s.endMediaTransition()
		}
	}()
	info, err := s.openPlayerSource(ctx, mediaPlayer, source)
	if err != nil {
		return err
	}
	state := s.waitForPlayerMedia(ctx, mediaPlayer, info.Duration)
	if err := mediaPlayer.Seek(ctx, 0); err != nil {
		return err
	}
	if state.Title != "" {
		identity.Media.Title = state.Title
	}
	if info.Title != "" {
		identity.Media.Title = info.Title
	}
	identity.Media.DurationSeconds = firstPositive(info.Duration, state.DurationSeconds)
	if identity.Path != "" {
		s.rememberSource(identity.Media.Fingerprint, identity.Path)
	}
	if err := client.SetMedia(protocol.MediaSet{Media: &identity.Media}); err != nil {
		return err
	}
	_, err = s.finishMediaTransition(ctx, mediaPlayer, state.Paused, func(paused bool) error {
		return client.SetPlayback(protocol.PlaybackSet{PositionSeconds: 0, Paused: paused, Rate: normalizedRate(state.Rate), Seek: true})
	})
	finished = true
	return err
}

func (s *Service) reopenMediaAtRoomClock(source string, paused bool) error {
	s.mediaLoadMu.Lock()
	defer s.mediaLoadMu.Unlock()
	client, mediaPlayer, err := s.ensurePlayer(playerStartExplicit)
	if err != nil {
		return err
	}
	identity, err := mediaid.Inspect(source, "", 0)
	if err != nil {
		return err
	}
	s.mu.RLock()
	if s.streamGateway != nil && source == s.streamGateway.URL() && s.streamIdentity != nil {
		identity.Media = *s.streamIdentity
	}
	s.mu.RUnlock()
	playback := localClockSnapshot(client).Playback
	ctx, cancel := s.mediaOperationContext(s.root)
	defer cancel()
	s.beginMediaTransition()
	finished := false
	defer func() {
		if !finished {
			s.endMediaTransition()
		}
	}()
	info, err := s.openPlayerSource(ctx, mediaPlayer, source)
	if err != nil {
		return err
	}
	state := s.waitForPlayerMedia(ctx, mediaPlayer, info.Duration)
	if err := mediaPlayer.Seek(ctx, playback.PositionSeconds); err != nil {
		return err
	}
	if err := mediaPlayer.SetRate(ctx, normalizedRate(playback.Rate)); err != nil {
		return err
	}
	if info.Title != "" {
		identity.Media.Title = info.Title
	} else if state.Title != "" {
		identity.Media.Title = state.Title
	}
	identity.Media.DurationSeconds = firstPositive(info.Duration, state.DurationSeconds)
	if identity.Path != "" {
		s.rememberSource(identity.Media.Fingerprint, identity.Path)
	}
	_, err = s.finishMediaTransition(ctx, mediaPlayer, paused, func(finalPaused bool) error {
		if err := client.SetMedia(protocol.MediaSet{Media: &identity.Media}); err != nil {
			return err
		}
		return client.SetPlayback(protocol.PlaybackSet{
			PositionSeconds: playback.PositionSeconds, Paused: finalPaused,
			Rate: normalizedRate(playback.Rate), Seek: true,
		})
	})
	finished = true
	return err
}

func (s *Service) LocatePlaylistItem(itemID, source string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	snapshot := client.Snapshot()
	var wanted *protocol.PlaylistItem
	for index := range snapshot.Playlist.Items {
		if snapshot.Playlist.Items[index].ID == itemID {
			wanted = &snapshot.Playlist.Items[index]
			break
		}
	}
	if wanted == nil || wanted.Media == nil {
		return errors.New("playlist item has no local media identity")
	}
	identity, err := mediaid.Inspect(source, wanted.Label, wanted.Media.DurationSeconds)
	if err != nil {
		return err
	}
	if identity.Media.Fingerprint != wanted.Media.Fingerprint {
		return errors.New("selected file does not match the room playlist item")
	}
	s.rememberSource(identity.Media.Fingerprint, identity.Path)
	s.mu.RLock()
	receivingThisFile := s.streamIdentity != nil && s.streamIdentity.Fingerprint == identity.Media.Fingerprint
	for _, pending := range s.pendingStreams {
		receivingThisFile = receivingThisFile || pending.media.Fingerprint == identity.Media.Fingerprint
	}
	s.mu.RUnlock()
	if receivingThisFile {
		_ = s.StopStreaming()
	}
	if snapshot.Playlist.Selected >= 0 && snapshot.Playlist.Selected < len(snapshot.Playlist.Items) && snapshot.Playlist.Items[snapshot.Playlist.Selected].ID == itemID {
		s.mu.Lock()
		s.selectedItem, s.selectedItemIdentity = "", ""
		s.mu.Unlock()
		s.applySelectedPlaylist(s.root, client)
	}
	return nil
}

func (s *Service) beginMediaTransition() {
	s.mediaTransitionMu.Lock()
	defer s.mediaTransitionMu.Unlock()
	s.mu.Lock()
	s.suppressUntil = time.Now().Add(3 * time.Second)
	s.openingMedia = true
	s.transitionPaused = nil
	s.lastSponsorEnd = 0
	s.mu.Unlock()
}

func (s *Service) endMediaTransition() {
	s.mediaTransitionMu.Lock()
	defer s.mediaTransitionMu.Unlock()
	s.clearMediaTransition()
}

func (s *Service) clearMediaTransition() {
	s.mu.Lock()
	s.suppressUntil = time.Time{}
	s.openingMedia = false
	s.transitionPaused = nil
	s.mu.Unlock()
}

func (s *Service) queueTransitionPause(paused bool) bool {
	s.mediaTransitionMu.Lock()
	defer s.mediaTransitionMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.openingMedia {
		return false
	}
	value := paused
	s.transitionPaused = &value
	return true
}

// finishMediaTransition serializes the last player command and authoritative
// publication with SetPaused. A pause that arrives before this method is
// queued and wins over the stale state captured when loading began; a pause
// that arrives afterwards observes openingMedia=false and runs normally.
func (s *Service) finishMediaTransition(ctx context.Context, mediaPlayer player.Player, fallbackPaused bool, publish func(bool) error) (bool, error) {
	s.mediaTransitionMu.Lock()
	defer s.mediaTransitionMu.Unlock()
	s.mu.RLock()
	queued := s.transitionPaused
	s.mu.RUnlock()
	paused := fallbackPaused
	if queued != nil {
		paused = *queued
	}
	err := (&trackedPlayer{Player: mediaPlayer, service: s}).SetPaused(ctx, paused)
	if err == nil && publish != nil {
		err = publish(paused)
	}
	s.clearMediaTransition()
	return paused, err
}

func (s *Service) openPlayerSource(ctx context.Context, mediaPlayer player.Player, source string) (youtube.Info, error) {
	s.mu.Lock()
	oldGateway, oldRequest := s.streamGateway, s.streamRequestID
	oldClient := s.client
	if oldGateway != nil && oldGateway.URL() != source {
		s.streamGateway, s.streamRequestID, s.streamReceiveItemID, s.streamIdentity = nil, "", "", nil
	} else {
		oldGateway = nil
	}
	s.mu.Unlock()
	if oldGateway != nil {
		oldGateway.Close()
		if oldClient != nil {
			_ = oldClient.RevokeStream(protocol.MediaStreamRevoke{RequestID: oldRequest, Reason: "viewer selected another source"})
		}
		s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "idle"}})
	}
	if !youtube.IsURL(source) {
		if err := mediaPlayer.Open(ctx, source); err != nil {
			return youtube.Info{}, err
		}
		s.mu.Lock()
		s.currentSource = source
		s.currentPlayerSource = source
		s.sponsorSegments = nil
		s.mu.Unlock()
		return youtube.Info{}, nil
	}
	s.mu.RLock()
	height := s.youtubeQualities[source]
	s.mu.RUnlock()
	stream, err := s.youtube.Resolve(ctx, source, height)
	if err != nil {
		return youtube.Info{}, err
	}
	if resolvedPlayer, ok := mediaPlayer.(player.ResolvedStreamPlayer); ok {
		err = resolvedPlayer.OpenResolved(ctx, player.ResolvedStream{
			VideoURL: stream.VideoURL, AudioURL: stream.AudioURL, CombinedURL: stream.CombinedURL,
		})
	} else if stream.CombinedURL != "" {
		err = mediaPlayer.Open(ctx, stream.CombinedURL)
	} else {
		err = errors.New("this player cannot combine separate high-quality YouTube video and audio streams")
	}
	if err != nil {
		fallbackResolver, supported := s.youtube.(interface {
			ResolveFallback(context.Context, string, int) (youtube.Stream, error)
		})
		if !supported {
			return youtube.Info{}, err
		}
		fallback, fallbackErr := fallbackResolver.ResolveFallback(ctx, source, height)
		if fallbackErr != nil {
			return youtube.Info{}, fmt.Errorf("high-quality YouTube stream failed to open (%v); web_safari fallback failed: %w", err, fallbackErr)
		}
		if resolvedPlayer, ok := mediaPlayer.(player.ResolvedStreamPlayer); ok {
			fallbackErr = resolvedPlayer.OpenResolved(ctx, player.ResolvedStream{CombinedURL: fallback.CombinedURL})
		} else {
			fallbackErr = mediaPlayer.Open(ctx, fallback.CombinedURL)
		}
		if fallbackErr != nil {
			return youtube.Info{}, fmt.Errorf("web_safari fallback failed to open: %w", fallbackErr)
		}
		stream = fallback
	}
	loadedState, _ := mediaPlayer.State(ctx)
	s.mu.Lock()
	s.currentSource = source
	s.currentPlayerSource = loadedState.Source
	s.sponsorSegments = append([]youtube.Segment(nil), stream.Info.Segments...)
	s.mu.Unlock()
	return stream.Info, nil
}

func (s *Service) YouTubeInfo(source string) (youtube.Info, error) {
	if !youtube.IsURL(source) {
		return youtube.Info{}, errors.New("a YouTube URL is required")
	}
	ctx, cancel := s.mediaOperationContext(s.root)
	defer cancel()
	return s.youtube.Inspect(ctx, source)
}

func (s *Service) SetYouTubeQuality(source string, height int) error {
	if !youtube.IsURL(source) {
		return errors.New("a YouTube URL is required")
	}
	if height < 0 || height > 8640 {
		return errors.New("YouTube quality must be Auto or a valid video height")
	}
	s.mu.Lock()
	s.youtubeQualities[source] = height
	s.mu.Unlock()
	return nil
}

func (s *Service) ChangeYouTubeQuality(source string, height int) error {
	if err := s.SetYouTubeQuality(source, height); err != nil {
		return err
	}
	s.mu.RLock()
	client, mediaPlayer, current := s.client, s.player, s.currentSource
	s.mu.RUnlock()
	if client == nil || current != source {
		return nil
	}
	if mediaPlayer == nil {
		return s.reopenMediaAtRoomClock(source, localClockSnapshot(client).Playback.Paused)
	}
	ctx, cancel := s.mediaOperationContext(s.root)
	defer cancel()
	state, err := mediaPlayer.State(ctx)
	if err != nil {
		return err
	}
	s.beginMediaTransition()
	finished := false
	defer func() {
		if !finished {
			s.endMediaTransition()
		}
	}()
	info, err := s.openPlayerSource(ctx, mediaPlayer, source)
	if err != nil {
		return err
	}
	s.waitForPlayerMedia(ctx, mediaPlayer, info.Duration)
	if err := mediaPlayer.Seek(ctx, state.PositionSeconds); err != nil {
		return err
	}
	if err := mediaPlayer.SetRate(ctx, normalizedRate(state.Rate)); err != nil {
		return err
	}
	_, err = s.finishMediaTransition(ctx, mediaPlayer, state.Paused, func(paused bool) error {
		return client.SetPlayback(protocol.PlaybackSet{
			PositionSeconds: state.PositionSeconds, Paused: paused,
			Rate: normalizedRate(state.Rate), Seek: true,
		})
	})
	finished = true
	return err
}

// waitForPlayerMedia bridges the asynchronous load semantics of mpv-family
// players. Open only acknowledges the load command; duration and chapters are
// populated shortly afterwards. Publishing the first state too early leaves
// the UI with a disabled timeline until the same file is opened again.
func (s *Service) waitForPlayerMedia(ctx context.Context, mediaPlayer player.Player, knownDuration float64) player.State {
	var latest player.State
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := mediaPlayer.State(ctx)
		if err == nil {
			latest = state
			if state.DurationSeconds > 0 || knownDuration > 0 && state.Source != "" {
				return latest
			}
		}
		select {
		case <-ctx.Done():
			return latest
		case <-deadline.C:
			return latest
		case <-ticker.C:
		}
	}
}

func (s *Service) SetSponsorBlockEnabled(enabled bool) {
	s.mu.Lock()
	s.sponsorBlock = enabled
	s.mu.Unlock()
}

func (s *Service) TimelineSegments() []TimelineSegment {
	s.mu.RLock()
	mediaPlayer := s.player
	sponsorSegments := append([]youtube.Segment(nil), s.sponsorSegments...)
	s.mu.RUnlock()

	result := make([]TimelineSegment, 0, len(sponsorSegments)+8)
	for _, segment := range sponsorSegments {
		result = append(result, TimelineSegment{
			StartSeconds: segment.Start, EndSeconds: segment.End, Title: segment.Title,
			Category: segment.Category, Kind: "sponsorblock",
		})
	}
	if mediaPlayer != nil {
		ctx, cancel := context.WithTimeout(s.root, 2*time.Second)
		state, err := mediaPlayer.State(ctx)
		cancel()
		if err == nil {
			for index, chapter := range state.Chapters {
				end := state.DurationSeconds
				if index+1 < len(state.Chapters) {
					end = state.Chapters[index+1].StartSeconds
				}
				title := strings.TrimSpace(chapter.Title)
				if title == "" {
					title = fmt.Sprintf("Chapter %d", index+1)
				}
				result = append(result, TimelineSegment{
					StartSeconds: chapter.StartSeconds, EndSeconds: end, Title: title, Kind: "chapter",
				})
			}
		}
	}
	slices.SortFunc(result, func(a, b TimelineSegment) int {
		switch {
		case a.StartSeconds < b.StartSeconds:
			return -1
		case a.StartSeconds > b.StartSeconds:
			return 1
		default:
			return 0
		}
	})
	return result
}

func firstPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func (s *Service) identifyAndRemember(source, title string, duration float64) (*protocol.Media, error) {
	identity, err := mediaid.Inspect(source, title, duration)
	if err != nil {
		return nil, err
	}
	if identity.Path != "" {
		s.rememberSource(identity.Media.Fingerprint, identity.Path)
	}
	return &identity.Media, nil
}

func (s *Service) rememberSource(fingerprint, source string) {
	if fingerprint == "" || source == "" {
		return
	}
	s.mu.Lock()
	s.sources[fingerprint] = source
	s.mu.Unlock()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Service) PlaylistAvailability() map[string]bool {
	s.mu.RLock()
	client := s.client
	sources := make(map[string]string, len(s.sources))
	for fingerprint, source := range s.sources {
		sources[fingerprint] = source
	}
	s.mu.RUnlock()
	result := make(map[string]bool)
	if client == nil {
		return result
	}
	for _, item := range client.Snapshot().Playlist.Items {
		present := false
		if item.Media != nil && sources[item.Media.Fingerprint] != "" {
			info, err := os.Stat(sources[item.Media.Fingerprint])
			present = err == nil && info.Mode().IsRegular()
		}
		result[item.ID] = item.URL != "" || present
	}
	s.publishAvailability(client, result)
	return result
}

func (s *Service) PlaylistExportLines() ([]string, error) {
	s.mu.RLock()
	client := s.client
	sources := make(map[string]string, len(s.sources))
	for fingerprint, source := range s.sources {
		sources[fingerprint] = source
	}
	s.mu.RUnlock()
	if client == nil {
		return nil, errors.New("not connected")
	}
	items := client.Snapshot().Playlist.Items
	lines := make([]string, 0, len(items))
	for _, item := range items {
		source := item.URL
		if source == "" && item.Media != nil {
			source = sources[item.Media.Fingerprint]
		}
		if source == "" {
			source = item.Label
		}
		lines = append(lines, source)
	}
	return lines, nil
}

func (s *Service) IndexMediaDirectory(directory string) (int, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return 0, errors.New("media directory is required")
	}
	count := 0
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !isMediaExtension(filepath.Ext(entry.Name())) {
			return nil
		}
		if count >= 10000 {
			return errors.New("media directory exceeds the 10000-file indexing limit")
		}
		identity, err := mediaid.Inspect(path, "", 0)
		if err != nil {
			return nil
		}
		s.rememberSource(identity.Media.Fingerprint, identity.Path)
		count++
		return nil
	})
	if err != nil {
		return count, fmt.Errorf("index media directory: %w", err)
	}
	// A joining participant commonly has no player yet: the selected room item
	// is what should start it. Retry from the connection after indexing rather
	// than requiring an already-open player.
	if client, connectionErr := s.connected(); connectionErr == nil {
		s.mu.Lock()
		s.selectedItem, s.selectedItemIdentity = "", ""
		s.mu.Unlock()
		s.applySelectedPlaylist(s.root, client)
	}
	return count, nil
}

func playlistItemIdentity(item protocol.PlaylistItem) string {
	if item.Media != nil && item.Media.Fingerprint != "" {
		return "media:" + item.Media.Fingerprint
	}
	return "url:" + item.URL
}

func isMediaExtension(extension string) bool {
	switch strings.ToLower(extension) {
	case ".mkv", ".mp4", ".webm", ".avi", ".mov", ".m4v", ".mp3", ".m4a", ".flac", ".ogg", ".wav", ".opus":
		return true
	default:
		return false
	}
}

func MediaFilesInDirectory(directory string, maximum int) ([]string, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return nil, errors.New("media directory is required")
	}
	if maximum <= 0 {
		maximum = 500
	}
	files := make([]string, 0)
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !isMediaExtension(filepath.Ext(entry.Name())) {
			return nil
		}
		if len(files) >= maximum {
			return errors.New("folder exceeds the 500-item playlist limit")
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return files, fmt.Errorf("read media directory: %w", err)
	}
	slices.Sort(files)
	return files, nil
}

func ExpandMediaPaths(paths []string, maximum int) ([]string, error) {
	if maximum <= 0 {
		maximum = 500
	}
	files := make([]string, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect dropped media: %w", err)
		}
		if !info.IsDir() {
			if isMediaExtension(filepath.Ext(path)) {
				files = append(files, path)
			}
			continue
		}
		remaining := maximum - len(files)
		if remaining <= 0 {
			return nil, errors.New("dropped media exceeds the 500-item playlist limit")
		}
		directoryFiles, err := MediaFilesInDirectory(path, remaining)
		if err != nil {
			return nil, err
		}
		files = append(files, directoryFiles...)
	}
	if len(files) > maximum {
		return nil, errors.New("dropped media exceeds the 500-item playlist limit")
	}
	return files, nil
}

// Closing the player must cancel URL resolution and loading immediately,
// without cancelling the room connection.
func (s *Service) mediaOperationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	s.mu.RLock()
	playerContext := s.playerContext
	s.mu.RUnlock()
	if playerContext == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(playerContext, cancel)
	return ctx, func() { stop(); cancel() }
}
