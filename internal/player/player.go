package player

import (
	"context"
	"time"
)

type Capabilities struct {
	PositionResolution time.Duration
	PlaybackRate       bool
	URLs               bool
}

type Chapter struct {
	StartSeconds float64 `json:"time"`
	Title        string  `json:"title"`
}

type State struct {
	PositionSeconds float64
	Paused          bool
	Buffering       bool
	Rate            float64
	DurationSeconds float64
	Title           string
	Source          string
	Chapters        []Chapter
	ObservedAt      time.Time
}

type EventKind string

const (
	EventState  EventKind = "state"
	EventMedia  EventKind = "media"
	EventClosed EventKind = "closed"
)

type Event struct {
	Kind  EventKind
	State State
	Err   error
}

// ResolvedStream is a set of already-extracted media URLs. Players that support
// separate tracks can load VideoURL and AudioURL without sacrificing quality.
type ResolvedStream struct {
	VideoURL    string
	AudioURL    string
	CombinedURL string
}

type ResolvedStreamPlayer interface {
	OpenResolved(context.Context, ResolvedStream) error
}

type Player interface {
	Capabilities() Capabilities
	State(context.Context) (State, error)
	SetPaused(context.Context, bool) error
	Seek(context.Context, float64) error
	SetRate(context.Context, float64) error
	Open(context.Context, string) error
	Events() <-chan Event
	Close() error
}
