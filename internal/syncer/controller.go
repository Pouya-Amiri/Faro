package syncer

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

const (
	defaultSoftDrift  = 120 * time.Millisecond
	defaultHardDrift  = time.Second
	maxRateCorrection = 0.03
)

type PlaybackSender interface {
	SetPlayback(protocol.PlaybackSet) error
}

type Controller struct {
	mu             sync.Mutex
	correctionRate float64
	nominalRate    float64
	player         player.Player
	sender         PlaybackSender
	serverNow      func() time.Time
	softDrift      time.Duration
	hardDrift      time.Duration
}

func New(mediaPlayer player.Player, sender PlaybackSender, serverNow func() time.Time) *Controller {
	if serverNow == nil {
		serverNow = time.Now
	}
	return &Controller{
		player: mediaPlayer, sender: sender, serverNow: serverNow,
		softDrift: defaultSoftDrift, hardDrift: defaultHardDrift,
	}
}

func (c *Controller) ApplyRemote(ctx context.Context, remote protocol.Playback, forceSeek bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	local, err := c.player.State(ctx)
	if err != nil {
		return err
	}
	if local.Buffering {
		return nil
	}
	desired := remote.PositionAt(c.serverNow())
	localPosition := local.PositionSeconds
	if !local.Paused && !local.ObservedAt.IsZero() {
		localPosition += time.Since(local.ObservedAt).Seconds() * local.Rate
	}
	drift := desired - localPosition
	softDrift, hardDrift := c.softDrift, c.hardDrift
	if resolution := c.player.Capabilities().PositionResolution; resolution >= time.Second {
		softDrift = resolution
		hardDrift = 2 * resolution
	}
	var failures []error
	if local.Paused != remote.Paused {
		failures = append(failures, c.player.SetPaused(ctx, remote.Paused))
	}
	if forceSeek || math.Abs(drift) >= hardDrift.Seconds() || remote.Paused && math.Abs(drift) >= softDrift.Seconds() {
		failures = append(failures, c.player.Seek(ctx, desired))
		drift = 0
	}
	if c.player.Capabilities().PlaybackRate {
		targetRate := remote.Rate
		if !remote.Paused && math.Abs(drift) >= softDrift.Seconds() {
			correction := clamp(drift*0.05, -maxRateCorrection, maxRateCorrection)
			targetRate *= 1 + correction
		}
		if math.Abs(local.Rate-targetRate) > 0.001 {
			err := c.player.SetRate(ctx, targetRate)
			failures = append(failures, err)
			if err == nil {
				c.correctionRate, c.nominalRate = targetRate, remote.Rate
			}
		}
	}
	return errors.Join(nonNil(failures)...)
}

func (c *Controller) PublishLocal(ctx context.Context, seek bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.player.State(ctx)
	if err != nil {
		return err
	}
	rate := state.Rate
	if c.nominalRate > 0 && math.Abs(rate-c.correctionRate) < .001 {
		rate = c.nominalRate
	}
	if rate <= 0 {
		rate = 1
	}
	return c.sender.SetPlayback(protocol.PlaybackSet{
		PositionSeconds: math.Max(0, projectedPosition(state)),
		Paused:          state.Paused, Rate: rate, Seek: seek,
	})
}

func clamp(value, minimum, maximum float64) float64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func nonNil(values []error) []error {
	result := values[:0]
	for _, value := range values {
		if value != nil {
			result = append(result, value)
		}
	}
	return result
}

func projectedPosition(state player.State) float64 {
	position := state.PositionSeconds
	if !state.Paused && !state.Buffering && !state.ObservedAt.IsZero() {
		position += time.Since(state.ObservedAt).Seconds() * state.Rate
	}
	return position
}
