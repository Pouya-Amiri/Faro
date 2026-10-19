package app

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/player/discovery"
	"github.com/Pouya-Amiri/Faro/internal/player/mpv"
	"github.com/Pouya-Amiri/Faro/internal/player/vlc"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/syncer"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

// DetectPlayer returns the launcher Faro would use for the selected player.
func DetectPlayer(name string) (string, error) { return discovery.Find(name) }

// DetectedPlayer reports where a supported player is installed. Executable
// is empty when it was not found.
type DetectedPlayer struct {
	Player     string `json:"player"`
	Executable string `json:"executable,omitempty"`
}

// DetectPlayers looks for every supported player at once and returns them in
// preference order, so the first one found is the best choice.
func DetectPlayers() []DetectedPlayer {
	players := discovery.SupportedPlayers()
	result := make([]DetectedPlayer, len(players))
	var wait sync.WaitGroup
	for index, name := range players {
		result[index].Player = name
		wait.Go(func() { result[index].Executable, _ = discovery.Find(name) })
	}
	wait.Wait()
	return result
}

// PlayerExecutable returns the launcher Faro will start for player: the
// executable the user chose, validated, or else the detected installation.
func PlayerExecutable(name, executable string) (string, error) {
	if strings.TrimSpace(executable) == "" {
		return discovery.Find(name)
	}
	return discovery.Executable(name, executable)
}

// SupportedPlayers returns the player profiles available on this platform.
func SupportedPlayers() []string { return discovery.SupportedPlayers() }

func validatePlayer(name string) error {
	if !discovery.Supported(name) {
		return fmt.Errorf("media player %q is not supported on this platform", name)
	}
	return nil
}

func startPlayer(ctx context.Context, request ConnectionRequest) (player.Player, error) {
	if err := validatePlayer(request.Player); err != nil {
		return nil, err
	}
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
				s.releasePlayer(mediaPlayer, "Player closed. Play an item to reopen it.", true)
			}
			return
		case event, ok := <-mediaPlayer.Events():
			if !ok {
				s.releasePlayer(mediaPlayer, "Player closed. Play an item to reopen it.", true)
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
				s.releasePlayer(mediaPlayer, message, true)
				return
			}
			client, _ := s.currentConnection()
			if client == nil {
				continue
			}
			if event.Kind == player.EventFailed && event.Err != nil {
				s.log("player: %v", event.Err)
				s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "player_failed", Message: event.Err.Error()}})
			}
			s.mu.RLock()
			openingMedia := s.openingMedia
			s.mu.RUnlock()
			if event.Kind == player.EventMedia && !openingMedia {
				s.publishPlayerMedia(ctx, client, mediaPlayer, event.State)
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

// publishPlayerMedia announces a media change the player reported outside
// Faro's own loads: the duration becoming known, a file opened in the
// player itself, or the file ending. The information belongs to the media
// generation current when it arrived, so it is dropped if a load starts
// meanwhile.
func (s *Service) publishPlayerMedia(ctx context.Context, client *faroclient.Client, mediaPlayer player.Player, state player.State) {
	if state.Source == "" {
		// Nothing is loaded any more. Events are delivered asynchronously, so
		// make sure that is still true before letting go of the media.
		stateCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		current, err := mediaPlayer.State(stateCtx)
		cancel()
		if err != nil || current.Source != "" {
			return
		}
		s.mu.Lock()
		if s.player != mediaPlayer || s.openingMedia || s.currentSource == "" {
			s.mu.Unlock()
			return
		}
		gen := s.invalidateMediaLocked()
		s.mu.Unlock()
		s.log("player: the media ended")
		_ = s.publishMedia(client, gen, nil)
		return
	}
	s.mu.Lock()
	gen := s.mediaGen
	var media *protocol.Media
	if stream := s.stream; stream != nil && stream.gateway != nil && state.Source == stream.gateway.URL() {
		if stream.media.DurationSeconds <= 0 && state.DurationSeconds > 0 {
			// The stream opened without a known duration and the player has
			// just found it.
			stream.media.DurationSeconds = state.DurationSeconds
		}
		identity := stream.media
		media = &identity
	}
	s.mu.Unlock()
	var err error
	if media == nil {
		identitySource := s.logicalSourceForPlayerEvent(state.Source)
		media, err = s.identifyAndRemember(identitySource, state.Title, state.DurationSeconds)
	}
	if err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "media_identity", Message: err.Error()}})
		return
	}
	if err := s.publishMedia(client, gen, media); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "media_publish", Message: err.Error()}})
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
		PositionSeconds: target, Paused: state.Paused, Rate: publishedRate(client, state.Rate), Seek: true, SponsorBlock: true,
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
	// A change of media is not a playback action: the jump to zero and the
	// stop when a file ends must not seek or pause everyone else.
	if s.openingMedia || time.Now().Before(s.suppressUntil) || previous.ObservedAt.IsZero() || current.Source != previous.Source {
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

// publishedRate converts the player's instantaneous rate, which may include a
// drift correction, into the rate to publish for the room.
func publishedRate(client playbackClient, observed float64) float64 {
	return syncer.NominalRate(normalizedRate(client.Snapshot().Playback.Rate), observed)
}

func normalizedRate(value float64) float64 {
	if value <= 0 {
		return 1
	}
	return value
}
