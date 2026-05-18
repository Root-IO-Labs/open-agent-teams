package backend

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// drain pulls every available frame from ch until the channel is either
// empty or has been closed. Returns whatever was received.
func drainChunkChan(t *testing.T, ch <-chan ChunkFrame, deadline time.Duration) []ChunkFrame {
	t.Helper()
	var got []ChunkFrame
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, f)
			timer.Reset(deadline)
		case <-timer.C:
			return got
		}
	}
}

func TestChunkBroadcaster_DeliversChunksToSubscriber(t *testing.T) {
	b := newChunkBroadcaster()
	defer b.Close()
	_, ch, cancel := b.Subscribe()
	defer cancel()

	b.Write([]byte("hello "))
	b.Write([]byte("world"))

	got := drainChunkChan(t, ch, 50*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2: %+v", len(got), got)
	}
	if !bytes.Equal(got[0].Chunk, []byte("hello ")) || !bytes.Equal(got[1].Chunk, []byte("world")) {
		t.Errorf("frame chunks = [%q, %q], want [\"hello \", \"world\"]", got[0].Chunk, got[1].Chunk)
	}
	if got[0].Gap != 0 || got[1].Gap != 0 {
		t.Errorf("expected Gap=0 for chunk frames, got [%d, %d]", got[0].Gap, got[1].Gap)
	}
}

// Each subscriber sees the same byte stream — fan-out is not load-balanced.
func TestChunkBroadcaster_FansOutToMultipleSubscribers(t *testing.T) {
	b := newChunkBroadcaster()
	defer b.Close()
	_, ch1, c1 := b.Subscribe()
	defer c1()
	_, ch2, c2 := b.Subscribe()
	defer c2()

	b.Write([]byte("abc"))
	b.Write([]byte("def"))

	g1 := drainChunkChan(t, ch1, 50*time.Millisecond)
	g2 := drainChunkChan(t, ch2, 50*time.Millisecond)
	for _, pair := range []struct {
		name string
		got  []ChunkFrame
	}{{"sub1", g1}, {"sub2", g2}} {
		if len(pair.got) != 2 {
			t.Errorf("%s: got %d frames, want 2", pair.name, len(pair.got))
			continue
		}
		if !bytes.Equal(pair.got[0].Chunk, []byte("abc")) || !bytes.Equal(pair.got[1].Chunk, []byte("def")) {
			t.Errorf("%s: chunks = [%q, %q], want [abc, def]", pair.name, pair.got[0].Chunk, pair.got[1].Chunk)
		}
	}
}

// Modifying the input slice after Write must NOT affect what the subscriber
// receives — broadcaster owns its own copy so the PTY reader can reuse its
// read buffer freely.
func TestChunkBroadcaster_CopiesInputSlice(t *testing.T) {
	b := newChunkBroadcaster()
	defer b.Close()
	_, ch, cancel := b.Subscribe()
	defer cancel()

	buf := []byte{'a', 'b', 'c'}
	b.Write(buf)
	buf[0] = 'Z' // simulate PTY reader overwriting its buffer

	got := drainChunkChan(t, ch, 50*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
	if !bytes.Equal(got[0].Chunk, []byte{'a', 'b', 'c'}) {
		t.Errorf("subscriber saw mutated chunk %q, want %q", got[0].Chunk, "abc")
	}
}

// A subscriber that never reads must not block the broadcaster — the rest
// of the pipeline (clean log, line broadcaster) keeps moving and the slow
// subscriber's drops accumulate into a gap counter.
func TestChunkBroadcaster_NonBlocking_DropsAccumulateGap(t *testing.T) {
	b := newChunkBroadcaster()
	defer b.Close()
	_, ch, cancel := b.Subscribe()
	defer cancel()

	// Saturate the subscriber's channel. With subscriberChunkChanSize=64,
	// the first 64 writes succeed; subsequent ones become pendingGap.
	for i := 0; i < subscriberChunkChanSize; i++ {
		b.Write([]byte{byte(i)})
	}
	// These writes have nowhere to go.
	dropped := 0
	for i := 0; i < 10; i++ {
		payload := []byte("ABCD") // 4 bytes each
		b.Write(payload)
		dropped += len(payload)
	}

	// Now make room and write one more so the gap marker can surface.
	// We need to drain a couple of frames to free up a slot.
	<-ch
	<-ch
	b.Write([]byte("X"))

	// Read until we see a gap frame. There must be exactly one with the
	// accumulated drop count, followed by the "X" chunk.
	deadline := time.NewTimer(200 * time.Millisecond)
	defer deadline.Stop()
	var gapFrame *ChunkFrame
	for gapFrame == nil {
		select {
		case f := <-ch:
			if f.Gap > 0 {
				ff := f
				gapFrame = &ff
			}
		case <-deadline.C:
			t.Fatalf("never received a gap frame; dropped %d bytes", dropped)
		}
	}
	if gapFrame.Gap != int64(dropped) {
		t.Errorf("gap = %d, want %d (the total dropped bytes)", gapFrame.Gap, dropped)
	}
	if gapFrame.TS.IsZero() {
		t.Errorf("gap frame TS must be the moment of the first drop, got zero time")
	}
}

// After Cancel(), the subscriber's channel is closed and Writes no longer
// reference the cancelled subscriber. We verify both via channel state and
// by writing post-cancel: the broadcaster must not panic on send-to-closed.
func TestChunkBroadcaster_CancelClosesChannel(t *testing.T) {
	b := newChunkBroadcaster()
	defer b.Close()
	_, ch, cancel := b.Subscribe()

	b.Write([]byte("before"))
	cancel()

	got := drainChunkChan(t, ch, 50*time.Millisecond)
	if len(got) != 1 || !bytes.Equal(got[0].Chunk, []byte("before")) {
		t.Errorf("pre-cancel frames = %+v, want exactly [\"before\"]", got)
	}
	// The channel must now be closed.
	if _, open := <-ch; open {
		t.Errorf("channel should be closed after cancel, but received another value")
	}

	// Post-cancel writes must not panic and must not block.
	done := make(chan struct{})
	go func() {
		b.Write([]byte("after"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Write after cancel hung — broadcaster still holds the cancelled subscriber")
	}
}

// Close() shuts the broadcaster down for everyone: all subscriber channels
// close, future Writes are no-ops, and a second Close is a no-op (idempotent).
func TestChunkBroadcaster_CloseClosesAllAndIsIdempotent(t *testing.T) {
	b := newChunkBroadcaster()
	_, ch1, c1 := b.Subscribe()
	defer c1()
	_, ch2, c2 := b.Subscribe()
	defer c2()

	b.Write([]byte("hi"))
	b.Close()
	b.Close() // second call must not panic

	for name, ch := range map[string]<-chan ChunkFrame{"sub1": ch1, "sub2": ch2} {
		// Drain any in-flight frames first.
		drainChunkChan(t, ch, 50*time.Millisecond)
		// Next receive must be the zero-value/closed signal.
		select {
		case _, open := <-ch:
			if open {
				t.Errorf("%s: channel should be closed after Close()", name)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("%s: receive from closed channel hung — Close didn't close it", name)
		}
	}

	// Post-close Writes must be no-ops, not panics.
	b.Write([]byte("more"))
}

// Race detector smoke test: concurrent Subscribe + Write + cancel must be
// safe. This is the configuration the daemon stream handler actually
// uses — bridges connect and disconnect at arbitrary times relative to
// PTY output.
func TestChunkBroadcaster_ConcurrentSubscribeAndWrite(t *testing.T) {
	b := newChunkBroadcaster()
	defer b.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer goroutine — pumps bytes constantly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := []byte("payload")
		for {
			select {
			case <-stop:
				return
			default:
				b.Write(buf)
			}
		}
	}()

	// Subscriber-churner — opens and cancels subscribers in a tight loop
	// while occasionally draining.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, ch, cancel := b.Subscribe()
			// Read a frame or two, then cancel.
			select {
			case <-ch:
			case <-time.After(5 * time.Millisecond):
			}
			cancel()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
