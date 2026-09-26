package syncer

import (
	"context"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

type fakePlayer struct {
	state  player.State
	seeks  []float64
	rates  []float64
	pauses []bool
}

func (f *fakePlayer) Capabilities() player.Capabilities {
	return player.Capabilities{PlaybackRate: true}
}
func (f *fakePlayer) State(context.Context) (player.State, error) { return f.state, nil }
func (f *fakePlayer) SetPaused(_ context.Context, value bool) error {
	f.pauses = append(f.pauses, value)
	f.state.Paused = value
	return nil
}
func (f *fakePlayer) Seek(_ context.Context, value float64) error {
	f.seeks = append(f.seeks, value)
	f.state.PositionSeconds = value
	return nil
}
func (f *fakePlayer) SetRate(_ context.Context, value float64) error {
	f.rates = append(f.rates, value)
	f.state.Rate = value
	return nil
}
func (f *fakePlayer) Open(context.Context, string) error { return nil }
func (f *fakePlayer) Events() <-chan player.Event        { return nil }
func (f *fakePlayer) Close() error                       { return nil }

type fakeSender struct{ values []protocol.PlaybackSet }

func (f *fakeSender) SetPlayback(value protocol.PlaybackSet) error {
	f.values = append(f.values, value)
	return nil
}

func TestHardDriftSeeks(t *testing.T) {
	now := time.Unix(100, 0)
	mediaPlayer := &fakePlayer{state: player.State{PositionSeconds: 2, Paused: false, Rate: 1, ObservedAt: time.Now()}}
	controller := New(mediaPlayer, &fakeSender{}, func() time.Time { return now })
	err := controller.ApplyRemote(context.Background(), protocol.Playback{
		PositionSeconds: 10, Paused: false, Rate: 1, UpdatedAtUnixMs: now.UnixMilli(),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mediaPlayer.seeks) != 1 || mediaPlayer.seeks[0] != 10 {
		t.Fatalf("expected seek to 10, got %#v", mediaPlayer.seeks)
	}
}

func TestSmallDriftUsesBoundedRateCorrection(t *testing.T) {
	now := time.Unix(100, 0)
	mediaPlayer := &fakePlayer{state: player.State{PositionSeconds: 9.5, Paused: false, Rate: 1, ObservedAt: time.Now()}}
	controller := New(mediaPlayer, &fakeSender{}, func() time.Time { return now })
	if err := controller.ApplyRemote(context.Background(), protocol.Playback{
		PositionSeconds: 10, Paused: false, Rate: 1, UpdatedAtUnixMs: now.UnixMilli(),
	}, false); err != nil {
		t.Fatal(err)
	}
	if len(mediaPlayer.seeks) != 0 {
		t.Fatalf("unexpected seek: %#v", mediaPlayer.seeks)
	}
	if len(mediaPlayer.rates) != 1 || mediaPlayer.rates[0] <= 1 || mediaPlayer.rates[0] > 1.03 {
		t.Fatalf("unexpected rate correction: %#v", mediaPlayer.rates)
	}
}

func TestPublishLocalUsesTypedCommand(t *testing.T) {
	sender := &fakeSender{}
	mediaPlayer := &fakePlayer{state: player.State{PositionSeconds: 42, Paused: true, Rate: 1.25}}
	controller := New(mediaPlayer, sender, nil)
	if err := controller.PublishLocal(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(sender.values) != 1 || sender.values[0].PositionSeconds != 42 || !sender.values[0].Seek {
		t.Fatalf("unexpected published state: %#v", sender.values)
	}
}

func TestCorrectionReturnsToAuthoritativeRate(t *testing.T) {
	now := time.Unix(100, 0)
	mediaPlayer := &fakePlayer{state: player.State{PositionSeconds: 9.5, Paused: false, Rate: 1, ObservedAt: time.Now()}}
	controller := New(mediaPlayer, &fakeSender{}, func() time.Time { return now })
	remote := protocol.Playback{PositionSeconds: 10, Paused: false, Rate: 1, UpdatedAtUnixMs: now.UnixMilli()}
	if err := controller.ApplyRemote(context.Background(), remote, false); err != nil {
		t.Fatal(err)
	}
	mediaPlayer.state.PositionSeconds = 10
	mediaPlayer.state.ObservedAt = time.Now()
	if err := controller.ApplyRemote(context.Background(), remote, false); err != nil {
		t.Fatal(err)
	}
	if got := mediaPlayer.rates[len(mediaPlayer.rates)-1]; got != 1 {
		t.Fatalf("correction rate was not restored: %v", mediaPlayer.rates)
	}
}

func TestPublishDoesNotTurnDriftCorrectionIntoRoomRate(t *testing.T) {
	now := time.Now()
	p := &fakePlayer{state: player.State{PositionSeconds: 9.5, Rate: 1, ObservedAt: now}}
	sender := &fakeSender{}
	c := New(p, sender, func() time.Time { return now })
	if err := c.ApplyRemote(context.Background(), protocol.Playback{PositionSeconds: 10, Rate: 1, UpdatedAtUnixMs: now.UnixMilli()}, false); err != nil {
		t.Fatal(err)
	}
	p.state.Paused = true
	if err := c.PublishLocal(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if sender.values[0].Rate != 1 {
		t.Fatalf("drift correction became room rate: %v", sender.values[0].Rate)
	}
}

func TestNominalRateKeepsDriftCorrectionOutOfTheRoom(t *testing.T) {
	cases := []struct{ room, observed, want float64 }{
		{1, 1.03, 1},      // maximum upward correction
		{1, 0.97, 1},      // maximum downward correction
		{1, 0.99, 1},      // partial correction
		{1.5, 1.545, 1.5}, // correction scales with the room rate
		{1, 1.25, 1.25},   // a deliberate change is kept
		{1, 1.1037, 1.1},  // and rounded to hundredths
		{0, 1.03, 1.03},   // no room rate known yet
		{1, 0, 1},         // missing player rate
	}
	for _, test := range cases {
		if got := NominalRate(test.room, test.observed); got != test.want {
			t.Errorf("NominalRate(%v, %v) = %v, want %v", test.room, test.observed, got, test.want)
		}
	}
}

func TestPublishAfterControllerRecreationUsesRoomRate(t *testing.T) {
	now := time.Now()
	// A reconnect creates a fresh controller while the player is still running
	// at the previous controller's correction rate.
	p := &fakePlayer{state: player.State{PositionSeconds: 10, Rate: 1.03, ObservedAt: now}}
	sender := &fakeSender{}
	c := New(p, sender, func() time.Time { return now })
	if err := c.ApplyRemote(context.Background(), protocol.Playback{PositionSeconds: 10, Paused: true, Rate: 1, UpdatedAtUnixMs: now.UnixMilli()}, false); err != nil {
		t.Fatal(err)
	}
	p.state.Rate = 1.03
	if err := c.PublishLocal(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := sender.values[len(sender.values)-1].Rate; got != 1 {
		t.Fatalf("published %v instead of the room rate", got)
	}
}
