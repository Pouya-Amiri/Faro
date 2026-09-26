package app

import (
	"context"
	"sync"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

// chatOverlayLines and chatOverlayDuration keep the overlay to a glance: the
// last few messages, each visible for a few seconds.
const chatOverlayLines = 4

var chatOverlayDuration = 8 * time.Second

// chatOverlay mirrors incoming chat in the player window. It has its own lock
// because player commands must not run under Service.mu.
type chatOverlay struct {
	mu       sync.Mutex
	disabled bool
	lines    []overlayLine
	timer    *time.Timer
	shown    bool
	// Each update replaces the last, so an update is skipped once a newer
	// generation exists; sendMu keeps the sends themselves in order.
	generation uint64
	sendMu     sync.Mutex
}

type overlayLine struct {
	line    player.ChatLine
	expires time.Time
}

// SetChatOverlayEnabled controls whether chat messages are shown over the
// video in players that support it.
func (s *Service) SetChatOverlayEnabled(enabled bool) {
	s.chat.mu.Lock()
	s.chat.disabled = !enabled
	if !enabled {
		s.chat.lines = nil
	}
	s.chat.mu.Unlock()
	if !enabled {
		s.refreshChatOverlay()
	}
}

func (s *Service) showChatInPlayer(message *protocol.ChatMessage) {
	s.chat.mu.Lock()
	if s.chat.disabled {
		s.chat.mu.Unlock()
		return
	}
	s.chat.lines = append(s.chat.lines, overlayLine{
		line:    player.ChatLine{Author: message.ParticipantName, Text: message.Message},
		expires: time.Now().Add(chatOverlayDuration),
	})
	if extra := len(s.chat.lines) - chatOverlayLines; extra > 0 {
		s.chat.lines = s.chat.lines[extra:]
	}
	s.chat.mu.Unlock()
	s.refreshChatOverlay()
}

// refreshChatOverlay drops expired lines, redraws the overlay and schedules
// the next expiry.
func (s *Service) refreshChatOverlay() {
	s.mu.RLock()
	target, _ := s.player.(player.ChatOverlayPlayer)
	s.mu.RUnlock()

	s.chat.mu.Lock()
	now := time.Now()
	kept := s.chat.lines[:0]
	for _, line := range s.chat.lines {
		if line.expires.After(now) {
			kept = append(kept, line)
		}
	}
	s.chat.lines = kept
	lines := make([]player.ChatLine, 0, len(kept))
	for _, line := range kept {
		lines = append(lines, line.line)
	}
	if s.chat.timer != nil {
		s.chat.timer.Stop()
		s.chat.timer = nil
	}
	if len(kept) > 0 {
		s.chat.timer = time.AfterFunc(time.Until(kept[0].expires), s.refreshChatOverlay)
	}
	// Nothing to draw and nothing on screen: skip the player round trip.
	skip := target == nil || len(lines) == 0 && !s.chat.shown
	if target != nil {
		s.chat.shown = len(lines) > 0
	}
	s.chat.generation++
	generation := s.chat.generation
	s.chat.mu.Unlock()
	if skip {
		return
	}
	go func() {
		s.chat.sendMu.Lock()
		defer s.chat.sendMu.Unlock()
		s.chat.mu.Lock()
		stale := s.chat.generation != generation
		s.chat.mu.Unlock()
		if stale {
			return
		}
		ctx, cancel := context.WithTimeout(s.root, 2*time.Second)
		defer cancel()
		_ = target.ShowChat(ctx, lines)
	}()
}
