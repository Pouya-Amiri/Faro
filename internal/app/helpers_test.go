package app

import (
	"errors"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/mediastream"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

// streamGatewayLocked returns the gateway of the stream the player plays,
// or nil. The caller holds s.mu.
func (s *Service) streamGatewayLocked() *mediastream.Gateway {
	if s.stream == nil || s.stream.phase != streamActive && s.stream.phase != streamResuming {
		return nil
	}
	return s.stream.gateway
}

// streamMediaLocked returns the media of the stream being received, or nil.
// The caller holds s.mu.
func (s *Service) streamMediaLocked() *protocol.Media {
	if s.stream == nil {
		return nil
	}
	return cloneStreamMedia(&s.stream.media)
}

func (s *Service) connectedPlayer() (*faroclient.Client, player.Player, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil || s.player == nil {
		return nil, nil, errors.New("no media player is open")
	}
	return s.client, s.player, nil
}
