package backend

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// makeEv is a test helper that builds a minimal sidecar.Event.
func makeEv(seq uint64, kind string) sidecar.Event {
	data, _ := json.Marshal(map[string]any{"i": seq})
	return sidecar.Event{
		V: 1, Seq: seq, TS: 1, Kind: kind, TurnID: "t", Data: data,
	}
}

func TestEventBroadcaster_SubscribeReceivesPublishes(t *testing.T) {
	bc := newEventBroadcaster()
	defer bc.Close()

	_, ch, catchup, cancel := bc.Subscribe()
	defer cancel()

	if len(catchup) != 0 {
		t.Fatalf("expected empty catchup on fresh broadcaster, got %d events", len(catchup))
	}

	go func() {
		for i := uint64(1); i <= 5; i++ {
			bc.Publish(makeEv(i, sidecar.KindAssistantDelta))
		}
	}()

	var got []sidecar.Event
	deadline := time.After(2 * time.Second)
	for len(got) < 5 {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d events", len(got))
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timeout at %d events", len(got))
		}
	}
	for i, ev := range got {
		if ev.Seq != uint64(i+1) {
			t.Errorf("events[%d].Seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestEventBroadcaster_CatchupReflectsPriorPublishes(t *testing.T) {
	bc := newEventBroadcaster()
	defer bc.Close()

	// Publish before anyone subscribes — a TUI joining mid-session
	// expects the ring-buffer catch-up to replay what it missed.
	for i := uint64(1); i <= 3; i++ {
		bc.Publish(makeEv(i, sidecar.KindAssistantDelta))
	}

	_, _, catchup, cancel := bc.Subscribe()
	defer cancel()

	if len(catchup) != 3 {
		t.Fatalf("catchup = %d events, want 3", len(catchup))
	}
	// Chronological order: oldest first.
	for i, ev := range catchup {
		if ev.Seq != uint64(i+1) {
			t.Errorf("catchup[%d].Seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestEventBroadcaster_CatchupAfterWrap(t *testing.T) {
	// Publish more than ringSize events; catchup should contain only
	// the last eventRingSize, in chronological order.
	bc := newEventBroadcaster()
	defer bc.Close()

	total := uint64(eventRingSize + 50)
	for i := uint64(1); i <= total; i++ {
		bc.Publish(makeEv(i, sidecar.KindAssistantDelta))
	}

	_, _, catchup, cancel := bc.Subscribe()
	defer cancel()

	if len(catchup) != eventRingSize {
		t.Fatalf("catchup = %d, want %d", len(catchup), eventRingSize)
	}
	// First catchup event should be the oldest-still-present.
	wantFirst := total - uint64(eventRingSize) + 1
	if catchup[0].Seq != wantFirst {
		t.Errorf("catchup[0].Seq = %d, want %d", catchup[0].Seq, wantFirst)
	}
	if catchup[len(catchup)-1].Seq != total {
		t.Errorf("catchup[last].Seq = %d, want %d", catchup[len(catchup)-1].Seq, total)
	}
}

func TestEventBroadcaster_MultipleSubscribersFanOut(t *testing.T) {
	bc := newEventBroadcaster()
	defer bc.Close()

	const nSubs = 3
	chans := make([]<-chan sidecar.Event, nSubs)
	cancels := make([]func(), nSubs)
	for i := 0; i < nSubs; i++ {
		_, ch, _, cancel := bc.Subscribe()
		chans[i] = ch
		cancels[i] = cancel
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	bc.Publish(makeEv(1, sidecar.KindAssistantDelta))
	bc.Publish(makeEv(2, sidecar.KindAssistantDelta))

	var wg sync.WaitGroup
	wg.Add(nSubs)
	for i := 0; i < nSubs; i++ {

		go func() {
			defer wg.Done()
			deadline := time.After(2 * time.Second)
			for j := 0; j < 2; j++ {
				select {
				case ev := <-chans[i]:
					if ev.Seq != uint64(j+1) {
						t.Errorf("sub[%d] event[%d].Seq = %d, want %d",
							i, j, ev.Seq, j+1)
					}
				case <-deadline:
					t.Errorf("sub[%d] timeout at event %d", i, j)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestEventBroadcaster_CancelStopsDelivery(t *testing.T) {
	bc := newEventBroadcaster()
	defer bc.Close()

	_, ch, _, cancel := bc.Subscribe()

	bc.Publish(makeEv(1, sidecar.KindAssistantDelta))
	select {
	case ev := <-ch:
		if ev.Seq != 1 {
			t.Errorf("got seq %d", ev.Seq)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout on first event")
	}

	cancel()
	// Channel should be closed after cancel.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after cancel")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancel did not close the channel")
	}
}

func TestEventBroadcaster_SlowSubscriberDoesNotBlockOthers(t *testing.T) {
	// This is the critical property: the rawBroadcaster's silent-drop
	// on full channel caused ghosting. The event broadcaster's per-
	// subscriber timeout must mean one slow sub doesn't starve others.
	bc := newEventBroadcaster()
	defer bc.Close()

	_, slowCh, _, cancelSlow := bc.Subscribe()
	defer cancelSlow()
	_, fastCh, _, cancelFast := bc.Subscribe()
	defer cancelFast()

	// Fill the slow subscriber's buffer without draining.
	// Fast subscriber drains normally.
	fastDone := make(chan int, 1)
	go func() {
		recv := 0
		deadline := time.After(3 * time.Second)
		for {
			select {
			case <-fastCh:
				recv++
				if recv == 20 {
					fastDone <- recv
					return
				}
			case <-deadline:
				fastDone <- recv
				return
			}
		}
	}()

	// Publish more events than fit in one sub's channel buffer. The
	// slow sub will drop some; the fast sub must still see all 20.
	for i := uint64(1); i <= 20; i++ {
		bc.Publish(makeEv(i, sidecar.KindAssistantDelta))
	}

	fastRecv := <-fastDone
	if fastRecv != 20 {
		t.Errorf("fast subscriber got %d of 20", fastRecv)
	}

	// Slow subscriber: we don't assert a specific count — just that
	// the producer wasn't blocked by it (the fast sub got all 20
	// within the deadline, which is the real property we care about).
	_ = slowCh
}

func TestEventBroadcaster_MetricsCountPublishes(t *testing.T) {
	bc := newEventBroadcaster()
	defer bc.Close()

	// Subscribe so Publish has a target.
	_, ch, _, cancel := bc.Subscribe()
	defer cancel()

	// Drain the channel in the background so we don't count as slow.
	go func() {
		for range ch {
		}
	}()

	for i := uint64(1); i <= 10; i++ {
		bc.Publish(makeEv(i, sidecar.KindAssistantDelta))
	}

	// Give the drain goroutine time to catch up so nothing lands in
	// slowSubs accidentally.
	time.Sleep(50 * time.Millisecond)

	pub, dropped, slow := bc.Metrics()
	if pub != 10 {
		t.Errorf("published = %d, want 10", pub)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 with fast subscriber", dropped)
	}
	if slow != 0 {
		t.Errorf("slow = %d, want 0 with fast subscriber", slow)
	}
}

func TestEventBroadcaster_CloseStopsPublish(t *testing.T) {
	bc := newEventBroadcaster()

	_, ch, _, _ := bc.Subscribe()

	bc.Publish(makeEv(1, sidecar.KindAssistantDelta))
	// Drain so the publish completes.
	<-ch

	bc.Close()

	// After close, channel should be drained/closed.
	select {
	case _, ok := <-ch:
		if ok {
			// Another event could have been queued before close; drain once more.
			select {
			case _, ok := <-ch:
				if ok {
					t.Error("channel should be closed after Close")
				}
			case <-time.After(100 * time.Millisecond):
				t.Error("channel not closed after Close")
			}
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("channel not closed after Close")
	}

	// Publish after Close is a no-op.
	bc.Publish(makeEv(2, sidecar.KindAssistantDelta))
	// Metrics should not have incremented for the post-close publish.
	pub, _, _ := bc.Metrics()
	if pub != 1 {
		t.Errorf("published = %d after close + noop publish, want 1", pub)
	}
}

func TestEventBroadcaster_CloseIsIdempotent(t *testing.T) {
	bc := newEventBroadcaster()
	bc.Close()
	bc.Close() // must not panic
}

func TestEventBroadcaster_SubscribeAfterClose(t *testing.T) {
	bc := newEventBroadcaster()
	bc.Close()
	_, ch, catchup, cancel := bc.Subscribe()
	defer cancel()
	if catchup != nil {
		t.Errorf("catchup should be nil after close, got %d events", len(catchup))
	}
	// Channel should already be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed for post-close Subscribe")
		}
	default:
		t.Error("channel should be drained immediately for post-close Subscribe")
	}
}
