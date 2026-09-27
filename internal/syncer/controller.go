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
	defaultSoftDrift = 120 * time.Millisecond
	defaultHardDrift = time.Second
	// syncTolerance is the drift reported as in sync; rate correction keeps
	// working on smaller drifts without anyone noticing them.
	syncTolerance     = 500 * time.Millisecond
	maxRateCorrection = 0.03
)

type PlaybackSender interface {
	SetPlayback(protocol.PlaybackSet) error
}

type Controller struct {
	mu             sync.Mutex
	correctionRate float64
	nominalRate    float64
	remoteRate     float64
	player         player.Player
	sender         PlaybackSender
	serverNow      func() time.Time
	softDrift      time.Duration
	hardDrift      time.Duration
	status         Status
}

// Status is how closely the local player followed the room at the last
// check, before any correction that check applied.
type Status struct {
	Buffering    bool
	DriftSeconds float64 // room position minus local position
	// ToleranceSeconds is the drift still perceived as in sync: half a
	// second, or two position steps for players that only report seconds.
	ToleranceSeconds float64
	MeasuredAt       time.Time
}

// InSync reports whether the last check found the player following the room.
func (s Status) InSync() bool {
	return !s.MeasuredAt.IsZero() && !s.Buffering && math.Abs(s.DriftSeconds) <= s.ToleranceSeconds
}

// Status returns the result of the last ApplyRemote check.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
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
	if remote.Rate > 0 {
		c.remoteRate = remote.Rate
	}
	local, err := c.player.State(ctx)
	if err != nil {
		return err
	}
	if local.Buffering {
		c.status = Status{Buffering: true, MeasuredAt: time.Now()}
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
	c.status = Status{DriftSeconds: drift, ToleranceSeconds: max(syncTolerance.Seconds(), hardDrift.Seconds()/2), MeasuredAt: time.Now()}
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
	} else {
		rate = NominalRate(c.remoteRate, rate)
	}
	return c.sender.SetPlayback(protocol.PlaybackSet{
		PositionSeconds: math.Max(0, projectedPosition(state)),
		Paused:          state.Paused, Rate: rate, Seek: seek,
	})
}

// NominalRate is the playback rate a participant should publish when their
// player reports observed. The controller nudges each player's speed by up to
// maxRateCorrection to remove drift, so a rate within that band of the room
// rate is the room rate, not a deliberate change; publishing it would turn a
// transient correction such as 1.03 into everybody's speed. Deliberate rates
// are rounded to hundredths.
func NominalRate(room, observed float64) float64 {
	if observed <= 0 {
		observed = 1
	}
	if room > 0 && math.Abs(observed/room-1) <= maxRateCorrection+0.0005 {
		return room
	}
	return math.Round(observed*100) / 100
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
