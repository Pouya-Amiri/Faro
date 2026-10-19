package player

import (
	"sync"
	"time"
)

// closeGrace is how long events still waiting when an adapter closes are
// offered to the app, so the reason a player closed is not lost.
const closeGrace = 2 * time.Second

// EventQueue delivers an adapter's events in order without ever blocking the
// adapter. Every event carries the player's complete state, so when the app
// falls behind a newer event replaces a waiting one of the same kind: state
// changes coalesce, and a media change can never be dropped.
type EventQueue struct {
	out    chan Event
	wake   chan struct{}
	stop   chan struct{}
	mu     sync.Mutex
	queue  []Event
	closed bool
}

func NewEventQueue() *EventQueue {
	q := &EventQueue{out: make(chan Event), wake: make(chan struct{}, 1), stop: make(chan struct{})}
	go q.deliver()
	return q
}

func (q *EventQueue) Events() <-chan Event { return q.out }

func (q *EventQueue) Push(event Event) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	replaced := false
	if event.Kind != EventClosed {
		for index := range q.queue {
			if q.queue[index].Kind == event.Kind {
				q.queue[index], replaced = event, true
				break
			}
		}
	}
	if !replaced {
		q.queue = append(q.queue, event)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Close stops accepting events. Those already queued are still offered for
// a short grace period.
func (q *EventQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	time.AfterFunc(closeGrace, func() { close(q.stop) })
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *EventQueue) deliver() {
	for {
		q.mu.Lock()
		if len(q.queue) == 0 {
			closed := q.closed
			q.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-q.wake:
			case <-q.stop:
				return
			}
			continue
		}
		event := q.queue[0]
		q.queue = q.queue[1:]
		q.mu.Unlock()
		select {
		case q.out <- event:
		case <-q.stop:
			return
		}
	}
}
