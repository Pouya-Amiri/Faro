package app

import (
	"context"
	"errors"
	"math"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/invite"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

func (s *Service) Snapshot() (protocol.Snapshot, error) {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil {
		return protocol.Snapshot{}, errors.New("not connected")
	}
	return localClockSnapshot(client), nil
}

func (s *Service) SendChat(message string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	return client.SendChat(message)
}

func (s *Service) SetRoomMode(mode protocol.RoomMode) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	return client.SetRoomMode(protocol.RoomModeSet{Mode: mode})
}

func (s *Service) SetRole(participantID string, role protocol.Role) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	return client.SetRole(protocol.RoomRoleSet{ParticipantID: participantID, Role: role})
}

// KickParticipant removes a participant from the room.
func (s *Service) KickParticipant(participantID string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	return client.KickParticipant(protocol.RoomParticipantKick{ParticipantID: participantID})
}

func (s *Service) SetPaused(paused bool) error {
	if !paused {
		s.resumeStreamingIntent()
	}
	client, mediaPlayer, err := s.playerWithMedia()
	if err != nil {
		client, err = s.connected()
		if err != nil {
			return err
		}
		source := s.sourceForPlayback(client)
		if !paused && source == "" {
			snapshot := client.Snapshot()
			if snapshot.Playlist.Selected < 0 || snapshot.Playlist.Selected >= len(snapshot.Playlist.Items) {
				s.mu.RLock()
				source = s.manualSource
				s.mu.RUnlock()
			} else {
				item := snapshot.Playlist.Items[snapshot.Playlist.Selected]
				if item.Media == nil {
					return errors.New("locate a playable copy of the selected item")
				}
				s.mu.Lock()
				s.playerDismissed = false
				s.mu.Unlock()
				s.reconcilePlaylistStreams(s.root, client)
				s.mu.RLock()
				requesting := s.stream != nil
				s.mu.RUnlock()
				if !requesting {
					return errors.New("locate a local copy or ask someone to offer the selected item")
				}
			}
		}
		if !paused && source == "" && client.Snapshot().Playlist.Selected < 0 {
			return errors.New("select an item before playing")
		}
		if !paused && source != "" {
			if err := s.reopenMediaAtRoomClock(source, paused); err != nil {
				return err
			}
			loadedClient, loadedPlayer, loadedErr := s.playerWithMedia()
			if loadedErr != nil {
				// A newer load took over and has not reached the player yet;
				// it takes the room's state when it finishes.
				return publishPaused(client, paused)
			}
			client, mediaPlayer = loadedClient, loadedPlayer
		} else {
			return publishPaused(client, paused)
		}
	}
	if s.queueTransitionPause(paused) {
		// Opening a wheel-selected item is a multi-command operation. Publishing
		// the user's intent immediately both records the pause in room activity
		// and lets the transition apply the newest state once loading finishes,
		// instead of racing a stale final "resume" command.
		return publishPaused(client, paused)
	}
	ctx, cancel := context.WithTimeout(s.root, 3*time.Second)
	defer cancel()
	s.syncApplyMu.Lock()
	defer s.syncApplyMu.Unlock()
	mediaPlayer = &trackedPlayer{Player: mediaPlayer, service: s}
	if err := mediaPlayer.SetPaused(ctx, paused); err != nil {
		return err
	}
	state, err := mediaPlayer.State(ctx)
	if err != nil {
		return err
	}
	return client.SetPlayback(protocol.PlaybackSet{PositionSeconds: state.PositionSeconds, Paused: paused, Rate: publishedRate(client, state.Rate)})
}

var errLiveControl = errors.New("this is a live broadcast: everyone watches from where they joined, so position and speed are not shared; seek in your own player to watch earlier")

// playingLive reports whether the player shows a live broadcast.
func (s *Service) playingLive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentLive
}

// publishPaused tells the room to pause or resume at its current position.
func publishPaused(client *faroclient.Client, paused bool) error {
	snapshot := localClockSnapshot(client)
	return client.SetPlayback(protocol.PlaybackSet{
		PositionSeconds: snapshot.Playback.PositionSeconds, Paused: paused,
		Rate: normalizedRate(snapshot.Playback.Rate),
	})
}

// sourceForPlayback resolves only the room's current queue selection. Explicit
// media opened outside the queue is tracked separately by manualSource.
func (s *Service) sourceForPlayback(client *faroclient.Client) string {
	item, ok := selectedPlaylistItem(client.Snapshot().Playlist)
	if !ok {
		return ""
	}
	if item.URL != "" {
		return item.URL
	}
	if item.Media == nil {
		return ""
	}
	if source := s.localSource(item.Media.Fingerprint); source != "" {
		return source
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if stream := s.stream; stream != nil && stream.gateway != nil && (stream.phase == streamActive || stream.phase == streamResuming) && stream.media.Fingerprint == item.Media.Fingerprint {
		return stream.gateway.URL()
	}
	return ""
}

// playerWithMedia returns the player when it shows media or is loading it.
// A player with nothing loaded, such as after the file ended, is treated
// like no player: room controls are published, and Play opens the media.
func (s *Service) playerWithMedia() (*faroclient.Client, player.Player, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil || s.player == nil || s.currentSource == "" && !s.openingMedia {
		return nil, nil, errors.New("no media is loaded")
	}
	return s.client, s.player, nil
}

func (s *Service) Seek(seconds float64) error {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return errors.New("seek position must be a number")
	}
	seconds = max(seconds, 0)
	client, mediaPlayer, err := s.playerWithMedia()
	if err != nil {
		// Like pause and speed, the position is room state: with no local
		// player it is still published, and players follow it when opened.
		client, err = s.connected()
		if err != nil {
			return err
		}
		snapshot := localClockSnapshot(client)
		return client.SetPlayback(protocol.PlaybackSet{
			PositionSeconds: seconds, Paused: snapshot.Playback.Paused,
			Rate: normalizedRate(snapshot.Playback.Rate), Seek: true,
		})
	}
	if s.playingLive() {
		return errLiveControl
	}
	ctx, cancel := context.WithTimeout(s.root, 3*time.Second)
	defer cancel()
	s.syncApplyMu.Lock()
	defer s.syncApplyMu.Unlock()
	mediaPlayer = &trackedPlayer{Player: mediaPlayer, service: s}
	if err := mediaPlayer.Seek(ctx, seconds); err != nil {
		return err
	}
	state, err := mediaPlayer.State(ctx)
	if err != nil {
		return err
	}
	return client.SetPlayback(protocol.PlaybackSet{PositionSeconds: seconds, Paused: state.Paused, Rate: publishedRate(client, state.Rate), Seek: true})
}

func (s *Service) SetRate(rate float64) error {
	if rate < MinPlaybackRate || rate > MaxPlaybackRate {
		return errors.New("playback rate must be between 0.25 and 4")
	}
	rate = math.Round(rate*100) / 100
	client, mediaPlayer, err := s.playerWithMedia()
	if err != nil {
		// Speed is room state: with no local player it is still published, and
		// every player (including this one, once opened) follows it.
		client, err = s.connected()
		if err != nil {
			return err
		}
		snapshot := localClockSnapshot(client)
		return client.SetPlayback(protocol.PlaybackSet{PositionSeconds: snapshot.Playback.PositionSeconds, Paused: snapshot.Playback.Paused, Rate: rate})
	}
	if s.playingLive() {
		return errLiveControl
	}
	ctx, cancel := context.WithTimeout(s.root, 3*time.Second)
	defer cancel()
	s.syncApplyMu.Lock()
	defer s.syncApplyMu.Unlock()
	mediaPlayer = &trackedPlayer{Player: mediaPlayer, service: s}
	if err := mediaPlayer.SetRate(ctx, rate); err != nil {
		return err
	}
	state, err := mediaPlayer.State(ctx)
	if err != nil {
		return err
	}
	return client.SetPlayback(protocol.PlaybackSet{PositionSeconds: state.PositionSeconds, Paused: state.Paused, Rate: rate})
}

func (s *Service) OwnerInvite() (string, error) {
	s.mu.RLock()
	value, ownerToken, hosted := s.invite, s.ownerToken, s.serverStatus
	s.mu.RUnlock()
	if hosted.Running && hosted.ShareInvite != "" {
		if shared, err := invite.Parse(hosted.ShareInvite); err == nil {
			value = shared
		}
	}
	if value.Address == "" {
		return "", errors.New("not connected")
	}
	value.OwnerToken = ownerToken
	return invite.Format(value)
}

func (s *Service) ParticipantInvite() (string, error) {
	s.mu.RLock()
	value, hosted := s.invite, s.serverStatus
	s.mu.RUnlock()
	if hosted.Running && hosted.ShareInvite != "" {
		if shared, err := invite.Parse(hosted.ShareInvite); err == nil {
			value = shared
		}
	}
	if value.Address == "" {
		return "", errors.New("not connected")
	}
	value.OwnerToken = ""
	return invite.Format(value)
}

// Playback rates every participant's player can follow: the room protocol
// accepts 0.25 to 4 times normal speed.
const (
	MinPlaybackRate = 0.25
	MaxPlaybackRate = 4.0
)

// RateRange describes the playback speeds the page may offer.
type RateRange struct {
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	Supported bool    `json:"supported"`
}

// PlaybackRateRange reports the speeds this participant can set. A player
// without rate control (for example a VLC build whose remote interface lacks
// the rate command) cannot change speed at all.
func (s *Service) PlaybackRateRange() RateRange {
	s.mu.RLock()
	mediaPlayer := s.player
	s.mu.RUnlock()
	supported := mediaPlayer == nil || mediaPlayer.Capabilities().PlaybackRate
	return RateRange{Min: MinPlaybackRate, Max: MaxPlaybackRate, Supported: supported}
}
