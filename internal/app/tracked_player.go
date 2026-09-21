package app

import (
	"context"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"math"
	"time"
)

// Match command echoes by value rather than ignoring all user input for a
// fixed interval after every drift check.
type trackedPlayer struct {
	player.Player
	service *Service
}

func (p *trackedPlayer) SetPaused(ctx context.Context, value bool) error {
	if state, err := p.Player.State(ctx); err == nil && state.Paused == value {
		return nil
	}
	p.service.mu.Lock()
	p.service.expectedPause = &value
	p.service.expectedUntil = time.Now().Add(2 * time.Second)
	p.service.mu.Unlock()
	return p.Player.SetPaused(ctx, value)
}
func (p *trackedPlayer) Seek(ctx context.Context, value float64) error {
	p.service.mu.Lock()
	p.service.expectedSeek = &value
	p.service.expectedUntil = time.Now().Add(2 * time.Second)
	p.service.mu.Unlock()
	return p.Player.Seek(ctx, value)
}
func (p *trackedPlayer) SetRate(ctx context.Context, value float64) error {
	if state, err := p.Player.State(ctx); err == nil && math.Abs(state.Rate-value) < .001 {
		return nil
	}
	p.service.mu.Lock()
	p.service.expectedRate = &value
	p.service.expectedUntil = time.Now().Add(2 * time.Second)
	p.service.mu.Unlock()
	return p.Player.SetRate(ctx, value)
}
