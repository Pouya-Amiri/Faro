package client

import (
	"sync"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

// maxQueuedEvents bounds events waiting for an app that stopped consuming.
// State notifications coalesce, so only messages such as chat and stream
// handshakes count towards it; reaching it means the app is wedged.
const maxQueuedEvents = 4096

// eventQueue delivers events to the app in arrival order without ever
// blocking the connection's reader, which must keep reading to stay alive.
//
// An event that carries nothing but its type only says that part of the
// snapshot changed, and the app reads the snapshot when it handles it. Such
// a notification is never dropped: while one of its type is still waiting,
// later changes of that type join it instead of queueing again. Every other
// event (chat, errors, wheel phases, stream handshakes) is queued as is.
type eventQueue struct {
	out      chan Event
	done     <-chan struct{}
	overflow func()

	mu      sync.Mutex
	pending []Event
	waiting map[protocol.MessageType]bool
	final   func() // runs once the queue has been delivered up to the final event
	wake    chan struct{}
}

func newEventQueue(done <-chan struct{}, overflow func()) *eventQueue {
	q := &eventQueue{
		out: make(chan Event), done: done, overflow: overflow,
		waiting: make(map[protocol.MessageType]bool), wake: make(chan struct{}, 1),
	}
	go q.deliver()
	return q
}

func (q *eventQueue) push(event Event) {
	q.mu.Lock()
	if q.final != nil {
		q.mu.Unlock()
		return
	}
	notification := isNotification(event)
	if notification && q.waiting[event.Type] {
		q.mu.Unlock()
		return
	}
	if len(q.pending) >= maxQueuedEvents {
		q.mu.Unlock()
		go q.overflow()
		return
	}
	if notification {
		q.waiting[event.Type] = true
	}
	q.pending = append(q.pending, event)
	q.mu.Unlock()
	q.signal()
}

// finish runs then once every queued event, and last if given, has been
// delivered. Later events are discarded.
func (q *eventQueue) finish(last *Event, then func()) {
	q.mu.Lock()
	if q.final != nil {
		q.mu.Unlock()
		return
	}
	if last != nil {
		q.pending = append(q.pending, *last)
	}
	q.final = then
	q.mu.Unlock()
	q.signal()
}

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) deliver() {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			final := q.final
			q.mu.Unlock()
			if final != nil {
				final()
				return
			}
			select {
			case <-q.wake:
				continue
			case <-q.done:
				return
			}
		}
		event := q.pending[0]
		q.pending[0] = Event{}
		q.pending = q.pending[1:]
		// The app reads the snapshot after this point, so a change made from
		// now on needs a notification of its own.
		delete(q.waiting, event.Type)
		q.mu.Unlock()
		select {
		case q.out <- event:
		case <-q.done:
			return
		}
	}
}

func isNotification(event Event) bool {
	return event.Error == nil && event.Chat == nil && event.Activity == nil && event.Wheel == nil &&
		event.StreamRequest == nil && event.StreamGrant == nil && event.StreamRevoked == nil
}
