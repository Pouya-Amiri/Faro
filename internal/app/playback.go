package app

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/player/discovery"
	"github.com/Pouya-Amiri/Faro/internal/player/mpv"
	"github.com/Pouya-Amiri/Faro/internal/player/vlc"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

// DetectPlayer returns the launcher Faro would use for the selected player.
func DetectPlayer(name string) (string, error) { return discovery.Find(name) }

func startPlayer(ctx context.Context, request ConnectionRequest) (player.Player, error) {
	switch strings.ToLower(strings.TrimSpace(request.Player)) {
	case "mpv", "":
		return mpv.Start(ctx, mpv.Config{Profile: mpv.ProfileMPV, Executable: request.Executable, ExtraArgs: request.PlayerArgs}, "")
	case "mpv.net", "mpvnet":
		return mpv.Start(ctx, mpv.Config{Profile: mpv.ProfileMPVNet, Executable: request.Executable, ExtraArgs: request.PlayerArgs}, "")
	case "iina":
		return mpv.Start(ctx, mpv.Config{Profile: mpv.ProfileIINA, Executable: request.Executable, ExtraArgs: request.PlayerArgs}, "")
	case "memento":
		return mpv.Start(ctx, mpv.Config{Profile: mpv.ProfileMemento, Executable: request.Executable, ExtraArgs: request.PlayerArgs}, "")
	case "vlc":
		return vlc.Start(ctx, vlc.Config{Executable: request.Executable, ExtraArgs: request.PlayerArgs}, "")
	default:
		return nil, fmt.Errorf("unsupported player %q", request.Player)
	}
}

func (s *Service) consumePlayer(ctx context.Context, mediaPlayer player.Player) {
	publishTicker := time.NewTicker(250 * time.Millisecond)
	defer publishTicker.Stop()
	var playerDone <-chan struct{}
	if lifecycle, ok := mediaPlayer.(interface{ Done() <-chan struct{} }); ok {
		playerDone = lifecycle.Done()
	}
	var pendingPlayback bool
	var pendingSeek bool
	for {
		select {
		case <-playerDone:
			if ctx.Err() == nil {
				s.releasePlayer(mediaPlayer, "Player closed. Play an item to reopen it.")
			}
			return
		case event, ok := <-mediaPlayer.Events():
			if !ok {
				s.releasePlayer(mediaPlayer, "Player closed. Play an item to reopen it.")
				return
			}
			if event.Kind == player.EventClosed {
				if ctx.Err() != nil {
					return
				}
				message := "Player closed. Faro is still connected and will reopen it when you play media."
				if event.Err != nil {
					message = "Player closed: " + event.Err.Error() + ". Faro is still connected."
				}
				s.releasePlayer(mediaPlayer, message)
				return
			}
			client, _ := s.currentConnection()
			if client == nil {
				continue
			}
			s.mu.RLock()
			openingMedia := s.openingMedia
			s.mu.RUnlock()
			if event.Kind == player.EventMedia && !openingMedia {
				s.mu.RLock()
				streamIdentity := cloneStreamMedia(s.streamIdentity)
				if s.streamGateway == nil || event.State.Source != s.streamGateway.URL() {
					streamIdentity = nil
				}
				s.mu.RUnlock()
				media := streamIdentity
				var err error
				if media == nil {
					identitySource := s.logicalSourceForPlayerEvent(event.State.Source)
					media, err = s.identifyAndRemember(identitySource, event.State.Title, event.State.DurationSeconds)
				}
				if err == nil {
					if err := client.SetMedia(protocol.MediaSet{Media: media}); err != nil {
						s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "media_publish", Message: err.Error()}})
					}
				} else if event.State.Source != "" {
					s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "media_identity", Message: err.Error()}})
				}
			}
			if !canControlPlayback(client.Snapshot()) {
				s.observeLocalChange(event.State)
				continue
			}
			if s.skipSponsorBlock(ctx, client, mediaPlayer, event.State) {
				pendingPlayback, pendingSeek = false, false
				continue
			}
			changed, seek := s.observeLocalChange(event.State)
			if changed {
				s.mu.Lock()
				s.localPlaybackPending = true
				s.mu.Unlock()
			}
			pendingPlayback = pendingPlayback || changed
			pendingSeek = pendingSeek || seek
		case <-publishTicker.C:
			if !pendingPlayback {
				continue
			}
			_, controller := s.currentConnection()
			if controller == nil {
				s.mu.Lock()
				s.localPlaybackPending = false
				s.mu.Unlock()
				pendingPlayback, pendingSeek = false, false
				continue
			}
			publishCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := controller.PublishLocal(publishCtx, pendingSeek)
			cancel()
			s.mu.Lock()
			s.localPlaybackPending = false
			s.mu.Unlock()
			if err != nil {
				s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "playback_publish", Message: err.Error()}})
			}
			// Whether accepted or rejected, the local command is no longer in
			// flight. On failure, return to authoritative room state instead of
			// suppressing remote playback or retrying stale player state forever.
			pendingPlayback, pendingSeek = false, false
		case <-ctx.Done():
			return
		}
	}
}

func (s *Service) logicalSourceForPlayerEvent(eventSource string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// mpv reports the temporary CDN URL loaded for YouTube playback. Keep the
	// stable video page URL as Faro's source so quality changes and player
	// recovery can resolve a fresh stream after signed URLs expire.
	if youtube.IsURL(s.currentSource) && eventSource != "" && eventSource == s.currentPlayerSource {
		return s.currentSource
	}
	if eventSource != "" && eventSource != s.currentSource {
		s.currentSource = eventSource
		s.sponsorSegments = nil
		s.lastSponsorEnd = 0
	}
	return eventSource
}

func (s *Service) skipSponsorBlock(ctx context.Context, client playbackClient, mediaPlayer player.Player, state player.State) bool {
	s.mu.Lock()
	if !s.sponsorBlock || s.openingMedia || state.PositionSeconds <= 0 {
		s.mu.Unlock()
		return false
	}
	var target float64
	for _, segment := range s.sponsorSegments {
		if state.PositionSeconds >= segment.Start && state.PositionSeconds < segment.End && segment.End != s.lastSponsorEnd {
			target = segment.End
			s.lastSponsorEnd = segment.End
			break
		}
	}
	if target > 0 {
		s.expectedSeek = &target
		s.expectedUntil = time.Now().Add(2 * time.Second)
	}
	s.mu.Unlock()
	if target == 0 || !canControlPlayback(client.Snapshot()) {
		return false
	}
	s.syncApplyMu.Lock()
	defer s.syncApplyMu.Unlock()
	seekCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := mediaPlayer.Seek(seekCtx, target); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "sponsorblock", Message: err.Error()}})
		return false
	}
	if err := client.SetPlayback(protocol.PlaybackSet{
		PositionSeconds: target, Paused: state.Paused, Rate: normalizedRate(state.Rate), Seek: true, SponsorBlock: true,
	}); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "sponsorblock", Message: err.Error()}})
	}
	return true
}

type playbackClient interface {
	Snapshot() protocol.Snapshot
	SetPlayback(protocol.PlaybackSet) error
}

func canControlPlayback(snapshot protocol.Snapshot) bool {
	if snapshot.Room.Mode == protocol.RoomCollaborative {
		return true
	}
	for _, participant := range snapshot.Participants {
		if participant.ID == snapshot.SelfID {
			return participant.Role == protocol.RoleOwner || participant.Role == protocol.RoleModerator
		}
	}
	return false
}

func mediaMatchesController(snapshot protocol.Snapshot) bool {
	var local, controller *protocol.Media
	for _, participant := range snapshot.Participants {
		if participant.ID == snapshot.SelfID {
			local = participant.Media
		}
		if participant.ID == snapshot.Playback.SetBy {
			controller = participant.Media
		}
	}
	if controller == nil && snapshot.Playlist.Selected >= 0 && snapshot.Playlist.Selected < len(snapshot.Playlist.Items) {
		controller = snapshot.Playlist.Items[snapshot.Playlist.Selected].Media
	}
	if controller == nil {
		for _, participant := range snapshot.Participants {
			if participant.Media != nil {
				controller = participant.Media
				break
			}
		}
	}
	return local != nil && controller != nil && local.Fingerprint != "" && local.Fingerprint == controller.Fingerprint
}

func (s *Service) observeLocalChange(current player.State) (bool, bool) {
	s.mu.Lock()
	previous := s.lastPlayer
	s.lastPlayer = current
	defer s.mu.Unlock()
	if s.openingMedia || time.Now().Before(s.suppressUntil) || previous.ObservedAt.IsZero() {
		return false, false
	}
	threshold := .75
	if s.player != nil && s.player.Capabilities().PositionResolution >= time.Second {
		threshold = 1.5
	}
	seek := !current.Buffering && !previous.Buffering && current.Source == previous.Source && math.Abs(current.PositionSeconds-expectedPosition(previous, current.ObservedAt)) > threshold
	pause := previous.Paused != current.Paused
	rate := math.Abs(previous.Rate-current.Rate) > 0.001
	if time.Now().Before(s.expectedUntil) {
		if pause && s.expectedPause != nil && current.Paused == *s.expectedPause {
			pause = false
			s.expectedPause = nil
		}
		if rate && s.expectedRate != nil && math.Abs(current.Rate-*s.expectedRate) < .005 {
			rate = false
			s.expectedRate = nil
		}
		if seek && s.expectedSeek != nil && math.Abs(current.PositionSeconds-*s.expectedSeek) < .75 {
			seek = false
			s.expectedSeek = nil
		}
	}
	return pause || rate || seek, seek
}

func expectedPosition(state player.State, now time.Time) float64 {
	if state.Paused || state.Buffering || state.ObservedAt.IsZero() {
		return state.PositionSeconds
	}
	return state.PositionSeconds + now.Sub(state.ObservedAt).Seconds()*state.Rate
}

func normalizedRate(value float64) float64 {
	if value <= 0 {
		return 1
	}
	return value
}
