package app

import (
	"context"
	"errors"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/invite"
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

func (s *Service) SetPaused(paused bool) error {
	client, mediaPlayer, err := s.connectedPlayer()
	if err != nil {
		client, err = s.connected()
		if err != nil {
			return err
		}
		s.mu.RLock()
		source := s.currentSource
		s.mu.RUnlock()
		if !paused && source != "" {
			if err := s.reopenMediaAtRoomClock(source, paused); err != nil {
				return err
			}
			client, mediaPlayer, err = s.connectedPlayer()
			if err != nil {
				return err
			}
		} else {
			snapshot := localClockSnapshot(client)
			return client.SetPlayback(protocol.PlaybackSet{
				PositionSeconds: snapshot.Playback.PositionSeconds, Paused: paused,
				Rate: normalizedRate(snapshot.Playback.Rate),
			})
		}
	}
	if s.queueTransitionPause(paused) {
		// Opening a wheel-selected item is a multi-command operation. Publishing
		// the user's intent immediately both records the pause in room activity
		// and lets the transition apply the newest state once loading finishes,
		// instead of racing a stale final "resume" command.
		snapshot := localClockSnapshot(client)
		return client.SetPlayback(protocol.PlaybackSet{
			PositionSeconds: snapshot.Playback.PositionSeconds, Paused: paused,
			Rate: normalizedRate(snapshot.Playback.Rate),
		})
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
	return client.SetPlayback(protocol.PlaybackSet{PositionSeconds: state.PositionSeconds, Paused: paused, Rate: normalizedRate(state.Rate)})
}

func (s *Service) Seek(seconds float64) error {
	client, mediaPlayer, err := s.connectedPlayer()
	if err != nil {
		return err
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
	return client.SetPlayback(protocol.PlaybackSet{PositionSeconds: seconds, Paused: state.Paused, Rate: normalizedRate(state.Rate), Seek: true})
}

func (s *Service) SetRate(rate float64) error {
	if rate < 0.25 || rate > 4 {
		return errors.New("playback rate must be between 0.25 and 4")
	}
	client, mediaPlayer, err := s.connectedPlayer()
	if err != nil {
		return err
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
