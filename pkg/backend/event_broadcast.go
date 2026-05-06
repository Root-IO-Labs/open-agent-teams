package backend

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// Event broadcaster tunables. Rationale on each:
//
//   - eventSubChanSize (512): per-subscriber channel buffer. Chat events
//     are message-level (one per LLM chunk aggregation), not byte-level,
//     so they're far less bursty than rawBroadcaster's PTY stream. 512
//     is generous without wasting memory; a typical TUI consumer drains
//     faster than a chat stream fills.
//
//   - eventRingSize (256): ring-buffer size for subscriber catch-up.
//     Enough to replay a couple of full turns for a TUI that
//     reconnects mid-session.
//
//   - eventSendTimeout (250ms): how long Publish waits on a full
//     subscriber channel before dropping the event FOR THAT SUBSCRIBER.
//     This is the key difference from rawBroadcaster: we prefer a
//     visible "slow subscriber" warning over silent-drop. 250ms is
//     longer than any plausible TUI tick interval but short enough that
//     the producer isn't stuck on a dead consumer.
const (
	eventSubChanSize = 512
	eventRingSize    = 256
	eventSendTimeout = 250 * time.Millisecond
)

// eventBroadcaster delivers sidecar.Event values to any number of
// subscribers. It mirrors rawBroadcaster's pub/sub shape but differs in
// three ways that matter for correctness:
//
//  1. Typed payload — sidecar.Event, not strings — so consumers don't
//     re-parse.
//  2. Bounded send with timeout on a slow subscriber, then drop-for-that-
//     subscriber with an atomic drop counter. No silent-drop of the
//     whole-channel kind that caused the 512-slot ghosting bug on the
//     rawBroadcaster side.
//  3. Ring-buffer catch-up, so a TUI subscribing mid-session sees the
//     last N events immediately without a replay from disk.
//
// eventBroadcaster is always allocated on every agent, even when the
// sidecar is off, because allocation cost is trivial (a struct and a
// small slice) and it simplifies the Subscribe API — callers don't
// need to handle "no broadcaster" vs "broadcaster exists but never
// receives" specially. With the sidecar off, Publish is simply never
// called and Subscribe returns a channel that never fires.
type eventBroadcaster struct {
	mu         sync.Mutex
	nextSubID  uint64
	subs       map[uint64]chan sidecar.Event
	ring       []sidecar.Event // circular buffer of recent events
	ringHead   int             // index where the next event will be written
	ringFilled bool            // true once ring has wrapped at least once
	closed     bool

	// Atomic counters for observability. Read from any goroutine without
	// taking the lock. Written from Publish (mutex-held) with a lock-free
	// counter update — using atomics keeps the hot path cheap.
	totalPublished atomic.Uint64
	totalDropped   atomic.Uint64
	slowSubs       atomic.Uint64
}

// newEventBroadcaster returns a ready-to-use broadcaster. Cheap to call.
func newEventBroadcaster() *eventBroadcaster {
	return &eventBroadcaster{
		subs: make(map[uint64]chan sidecar.Event),
		ring: make([]sidecar.Event, eventRingSize),
	}
}

// Publish delivers ev to every current subscriber. Uses a bounded send
// per subscriber (eventSendTimeout) — a slow subscriber drops its own
// event, increments slowSubs, and publication continues to other subs.
// Total-order is preserved per-subscriber.
func (b *eventBroadcaster) Publish(ev sidecar.Event) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}

	// Add to ring buffer first so a new subscriber calling Subscribe()
	// a nanosecond after we return sees this event in its catch-up.
	b.ring[b.ringHead] = ev
	b.ringHead = (b.ringHead + 1) % eventRingSize
	if b.ringHead == 0 {
		b.ringFilled = true
	}
	b.totalPublished.Add(1)

	// Snapshot subscriber list under the lock, then deliver outside it
	// so a slow subscriber can't block others past the per-send timeout.
	targets := make([]chan sidecar.Event, 0, len(b.subs))
	for _, ch := range b.subs {
		targets = append(targets, ch)
	}
	b.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- ev:
			// Delivered.
		default:
			// Fast-path full channel: try again with a short timeout.
			// Using a timer per-subscriber rather than a shared one so
			// one slow subscriber doesn't consume another's grace window.
			timer := time.NewTimer(eventSendTimeout)
			select {
			case ch <- ev:
				timer.Stop()
			case <-timer.C:
				// Subscriber drained nothing in 250ms — drop for them
				// only. Counter is visible via SlowSubscribers().
				b.totalDropped.Add(1)
				b.slowSubs.Add(1)
			}
		}
	}
}

// Subscribe returns a receive-only channel that future Publish calls
// will deliver to, a cancel function that removes the subscription and
// closes the channel, and a slice of the current ring-buffer contents
// in chronological order for immediate catch-up.
//
// Callers MUST drain the channel (or call cancel) to avoid slow-
// subscriber drops. Cancel is idempotent.
func (b *eventBroadcaster) Subscribe() (uint64, <-chan sidecar.Event, []sidecar.Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		// Return a closed channel and empty catch-up so the caller's
		// range loop exits cleanly.
		ch := make(chan sidecar.Event)
		close(ch)
		return 0, ch, nil, func() {}
	}

	id := b.nextSubID
	b.nextSubID++
	ch := make(chan sidecar.Event, eventSubChanSize)
	b.subs[id] = ch

	catchup := b.snapshotRingLocked()

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		sub, ok := b.subs[id]
		if !ok {
			return
		}
		delete(b.subs, id)
		close(sub)
	}
	return id, ch, catchup, cancel
}

// Close stops accepting new publishes, closes every subscriber channel,
// and releases the ring. Subsequent Publish calls are no-ops. Safe to
// call multiple times.
func (b *eventBroadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subs {
		close(ch)
		delete(b.subs, id)
	}
}

// Metrics returns observability counters. All three fields are
// cumulative since broadcaster creation.
func (b *eventBroadcaster) Metrics() (published, dropped, slow uint64) {
	return b.totalPublished.Load(), b.totalDropped.Load(), b.slowSubs.Load()
}

// snapshotRingLocked returns the ring contents in chronological order
// — the oldest event first, newest last. Caller must hold b.mu.
func (b *eventBroadcaster) snapshotRingLocked() []sidecar.Event {
	if !b.ringFilled && b.ringHead == 0 {
		return nil
	}
	if !b.ringFilled {
		// Ring hasn't wrapped yet — events are at indices [0, ringHead).
		out := make([]sidecar.Event, b.ringHead)
		copy(out, b.ring[:b.ringHead])
		return out
	}
	// Ring has wrapped — oldest event is at ringHead, newest at (ringHead-1).
	out := make([]sidecar.Event, 0, eventRingSize)
	out = append(out, b.ring[b.ringHead:]...)
	out = append(out, b.ring[:b.ringHead]...)
	return out
}
