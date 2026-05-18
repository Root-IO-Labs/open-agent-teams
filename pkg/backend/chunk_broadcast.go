package backend

import (
	"sync"
	"time"
)

// chunkBroadcaster fans out raw, unmodified PTY byte chunks to N subscribers.
// Distinct from rawBroadcaster (which ANSI-strips, dedups, and line-buffers
// for the TUI streaming path) — this one preserves every byte, including
// ANSI escapes, because the side-panel debug view needs them and the bridge
// uses byte timing/volume as the heartbeat signal for pretty-mode activity.
//
// The plan calls for a per-subscriber 256 KiB backlog cap with a "gap"
// marker when the cap is exceeded. We approximate that with a fixed-size
// frame channel of subscriberChunkChanSize, sized so that with the typical
// 4 KiB read buffer used by the PTY reader the in-flight bytes never exceed
// the cap by more than one read worth (~+4 KiB worst case). When the channel
// is full, dropped bytes accumulate into a per-subscriber pendingGap counter
// and surface to the client as a {Gap: N, TS: t} frame the next time the
// channel has room.
//
// Backpressure is therefore eventually-consistent: a slow client misses
// bytes (with an accurate count), the PTY reader never blocks, and the
// other live broadcasters (clean log + line-based stream) keep going.
type chunkBroadcaster struct {
	mu     sync.Mutex
	subs   map[uint64]*chunkSubscriber
	nextID uint64
	closed bool
}

// ChunkFrame is what subscribers receive from a chunkBroadcaster.
// Exactly one of Chunk and Gap is set per frame:
//   - Chunk frames carry raw PTY bytes that arrived in this slice (the
//     slice is a fresh copy — the broadcaster does not retain or share
//     the underlying array with the PTY reader).
//   - Gap frames signal that N bytes were dropped between this frame
//     and the previous chunk frame because the subscriber's channel
//     was full. Clients render this to the user (e.g. "[256 KiB dropped]")
//     so they know they're missing data rather than reading a contiguous
//     stream that's secretly truncated.
//
// TS is the wall-clock UTC time the broadcaster observed the chunk
// (or the moment the gap was first recorded). It's used by the daemon
// stream handler for client-side latency display, not by the
// broadcaster itself.
type ChunkFrame struct {
	Chunk []byte
	Gap   int64
	TS    time.Time
}

// subscriberChunkChanSize is the per-subscriber frame channel capacity.
// With the typical 4 KiB PTY read buffer this approximates the plan's
// 256 KiB backlog threshold before chunks start getting dropped. Tuning
// note: bigger means more memory burn per slow subscriber but fewer gap
// markers; smaller means more aggressive backpressure but at the cost of
// dropping chunks during legitimate short bursts. 64 is a deliberate
// balance: holds ~1s of a normally chatty agent's output, drops fast
// when a subscriber wedges.
const subscriberChunkChanSize = 64

type chunkSubscriber struct {
	ch chan ChunkFrame

	// Per-subscriber accumulator for bytes that we tried to send but
	// couldn't because the channel was full. Surfaces to the client as
	// a Gap frame on the first subsequent successful send. Protected
	// by the parent chunkBroadcaster.mu — we deliberately don't add a
	// per-subscriber mutex because every mutator already runs under
	// the broadcaster mu (Write iterates subs, Subscribe/cancel rewrite
	// the subs map), so the extra lock would only serialize identical
	// callers.
	pendingGap int64
	pendingTS  time.Time
}

func newChunkBroadcaster() *chunkBroadcaster {
	return &chunkBroadcaster{
		subs: make(map[uint64]*chunkSubscriber),
	}
}

// Write fans out one PTY read to all live subscribers. Non-blocking:
// channels that are full are skipped, with the byte count accumulated
// into pendingGap. The PTY reader never waits on a subscriber.
//
// The input slice is owned by the PTY reader and reused on every read,
// so we copy before broadcasting. Allocation cost is one make per
// Write call — acceptable because the typical agent emits at most
// a few hundred chunks per second.
func (b *chunkBroadcaster) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.subs) == 0 {
		return
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	now := time.Now().UTC()

	for _, sub := range b.subs {
		// Flush any accumulated gap first so the gap marker shows up
		// adjacent to the next live chunk, not after a long quiet period.
		if sub.pendingGap > 0 {
			select {
			case sub.ch <- ChunkFrame{Gap: sub.pendingGap, TS: sub.pendingTS}:
				sub.pendingGap = 0
				sub.pendingTS = time.Time{}
			default:
				// Channel still full — keep accumulating. The gap will
				// surface eventually; in the meantime the chunk we're
				// about to try will probably fail too and be added.
			}
		}
		select {
		case sub.ch <- ChunkFrame{Chunk: cp, TS: now}:
		default:
			// Subscriber is backed up — record the drop and move on.
			// Stamp the gap window with the moment of the first drop so
			// the gap frame TS lines up with when the data was lost, not
			// when the channel eventually drains.
			if sub.pendingGap == 0 {
				sub.pendingTS = now
			}
			sub.pendingGap += int64(len(cp))
		}
	}
}

// Subscribe registers a new subscriber. The returned channel receives
// ChunkFrame values until cancel() is called (the channel is closed) or
// the broadcaster's Close() is called (the channel is closed).
//
// New subscribers start with an empty buffer — there's no ring buffer
// catch-up like rawBroadcaster has, because the chunk stream's whole
// purpose is "everything from now on, byte-accurate." A side-panel chat
// subscriber that opens late simply doesn't see history; the log file
// is the historical record.
func (b *chunkBroadcaster) Subscribe() (uint64, <-chan ChunkFrame, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.nextID
	b.nextID++
	sub := &chunkSubscriber{
		ch: make(chan ChunkFrame, subscriberChunkChanSize),
	}
	b.subs[id] = sub

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subs[id]; ok && existing == sub {
			delete(b.subs, id)
			close(sub.ch)
		}
	}
	return id, sub.ch, cancel
}

// Close terminates the broadcaster: future Writes are no-ops, all live
// subscriber channels are closed. Idempotent.
func (b *chunkBroadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, sub := range b.subs {
		delete(b.subs, id)
		close(sub.ch)
	}
}
