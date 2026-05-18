package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// langfuseTracer batches ingestion events on a background worker. The hot path
// only ever does an atomic enqueue; HTTP I/O happens off the agent's path.
//
// Wire format follows Langfuse's `/api/public/ingestion` endpoint, which
// accepts a list of `{id, type, timestamp, body}` events. Documented at
// https://api.reference.langfuse.com/#tag/Ingestion.
type langfuseTracer struct {
	cfg        Config
	host       string
	sampleRate float64
	httpClient *http.Client

	queue   chan ingestionEvent
	wg      sync.WaitGroup
	closeCh chan struct{}
	closed  atomic.Bool

	// warnOnce gates the single "couldn't reach Langfuse" log per session so a
	// failed connection doesn't spam logs.
	warnOnce sync.Once
}

// queueCapacity bounds in-memory buffering. Sized so a normal session never
// blocks; a stuck Langfuse endpoint will start dropping spans after this many,
// not blocking the agent path.
const queueCapacity = 4096

// flushInterval upper-bounds latency between span creation and delivery.
const flushInterval = 3 * time.Second

// batchSize is the max events per ingestion call.
const batchSize = 64

func newLangfuseTracer(cfg Config, host string, rate float64) *langfuseTracer {
	t := &langfuseTracer{
		cfg:        cfg,
		host:       strings.TrimRight(host, "/"),
		sampleRate: rate,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		queue:      make(chan ingestionEvent, queueCapacity),
		closeCh:    make(chan struct{}),
	}
	t.wg.Add(1)
	go t.worker()
	return t
}

// ingestionEvent is the wire envelope Langfuse expects.
type ingestionEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`      // "trace-create" / "span-create" / "span-update" / "event-create" / "generation-create"
	Timestamp string         `json:"timestamp"` // ISO-8601 with millis
	Body      map[string]any `json:"body"`
}

func (t *langfuseTracer) NewTrace(ctx context.Context, name string, metadata map[string]any) (context.Context, string) {
	if t.closed.Load() {
		return ctx, ""
	}
	id := newID()
	body := map[string]any{
		"id":      id,
		"name":    name,
		"release": t.cfg.Release,
	}
	if metadata != nil {
		body["metadata"] = metadata
	}
	t.enqueue(ingestionEvent{
		ID:        newID(),
		Type:      "trace-create",
		Timestamp: nowISO(),
		Body:      body,
	})
	return WithTraceID(ctx, id), id
}

func (t *langfuseTracer) Router(ctx context.Context, ev RouterEvent) {
	if t.closed.Load() || !t.sample() {
		return
	}
	trace := TraceIDFromContext(ctx)
	t.enqueue(ingestionEvent{
		ID:        newID(),
		Type:      "event-create",
		Timestamp: nowISO(),
		Body: map[string]any{
			"id":      newID(),
			"traceId": trace,
			"name":    "router_decision",
			"metadata": map[string]any{
				"task_text_hash": ev.TaskTextHash,
				"task_text_len":  ev.TaskTextLen,
				"complexity":     ev.Complexity,
				"candidates":     ev.Candidates,
				"chosen_model":   ev.ChosenModel,
				"floor_met":      ev.FloorMet,
				"input_price_us": ev.InputPriceUS,
				"reason":         Scrub(ev.Reason),
			},
		},
	})
}

func (t *langfuseTracer) AgentStart(ctx context.Context, ev AgentEvent) {
	if t.closed.Load() || !t.sample() {
		return
	}
	trace := TraceIDFromContext(ctx)
	t.enqueue(ingestionEvent{
		ID:        newID(),
		Type:      "event-create",
		Timestamp: nowISO(),
		Body: map[string]any{
			"id":      newID(),
			"traceId": trace,
			"name":    "agent_start",
			"metadata": map[string]any{
				"agent_id":       ev.AgentID,
				"agent_type":     ev.AgentType,
				"repo":           ev.RepoName,
				"model":          ev.Model,
				"routing_source": ev.RoutingSource,
			},
		},
	})
}

func (t *langfuseTracer) AgentEnd(ctx context.Context, ev AgentExit) {
	if t.closed.Load() || !t.sample() {
		return
	}
	trace := TraceIDFromContext(ctx)
	t.enqueue(ingestionEvent{
		ID:        newID(),
		Type:      "event-create",
		Timestamp: nowISO(),
		Body: map[string]any{
			"id":      newID(),
			"traceId": trace,
			"name":    "agent_end",
			"level":   levelForExit(ev.Reason),
			"metadata": map[string]any{
				"agent_id":      ev.AgentID,
				"reason":        ev.Reason,
				"exit_code":     ev.ExitCode,
				"input_tokens":  ev.InputTokens,
				"output_tokens": ev.OutputTokens,
			},
		},
	})
}

func levelForExit(reason string) string {
	switch reason {
	case "success", "removed":
		return "DEFAULT"
	case "crashed", "timeout":
		return "ERROR"
	case "killed", "cancelled":
		return "WARNING"
	default:
		return "DEFAULT"
	}
}

func (t *langfuseTracer) Flush(timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		if len(t.queue) == 0 {
			return nil
		}
		select {
		case <-deadline:
			return fmt.Errorf("flush timed out with %d events queued", len(t.queue))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (t *langfuseTracer) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(t.closeCh)
	t.wg.Wait()
	return nil
}

// enqueue is non-blocking. Dropped events on a full queue are logged once.
func (t *langfuseTracer) enqueue(ev ingestionEvent) {
	select {
	case t.queue <- ev:
	default:
		t.warnOnce.Do(func() {
			log.Printf("telemetry: queue full, dropping events (Langfuse slow or unreachable)")
		})
	}
}

func (t *langfuseTracer) sample() bool {
	if t.sampleRate >= 1.0 {
		return true
	}
	// Cheap rand: lower 16 bits of a fresh ID hash.
	var b [2]byte
	_, _ = rand.Read(b[:])
	r := float64(uint16(b[0])<<8|uint16(b[1])) / 65535.0
	return r < t.sampleRate
}

func (t *langfuseTracer) worker() {
	defer t.wg.Done()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	batch := make([]ingestionEvent, 0, batchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		t.send(batch)
		batch = batch[:0]
	}

	for {
		select {
		case <-t.closeCh:
			// Drain remaining events on shutdown.
			for {
				select {
				case ev := <-t.queue:
					batch = append(batch, ev)
					if len(batch) >= batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case ev := <-t.queue:
			batch = append(batch, ev)
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (t *langfuseTracer) send(batch []ingestionEvent) {
	body, err := json.Marshal(map[string]any{"batch": batch})
	if err != nil {
		return // unreachable in practice; ingestion structs are JSON-clean
	}
	url := t.host + "/api/public/ingestion"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.SetBasicAuth(t.cfg.PublicKey, t.cfg.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "oat-telemetry/1")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		t.warnOnce.Do(func() {
			log.Printf("telemetry: Langfuse send failed: %v (further errors suppressed this session)", err)
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// 4xx = our payload is wrong; 5xx = Langfuse problem. Either way drop
		// silently after one warning so we don't loop.
		t.warnOnce.Do(func() {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			log.Printf("telemetry: Langfuse %d: %s (further errors suppressed)", resp.StatusCode, snippet)
		})
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}

// Ping verifies that cfg can authenticate against the configured Langfuse
// endpoint. It sends one ingestion batch synchronously and reports a non-nil
// error on network failure, 4xx (auth/permission), or 5xx (Langfuse outage).
//
// Used by `oat telemetry setup` to validate keys before persisting them. Does
// not require a Tracer instance; safe to call on its own.
func Ping(cfg Config) error {
	host := cfg.Host
	if host == "" {
		host = "https://cloud.langfuse.com"
	}
	host = strings.TrimRight(host, "/")

	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		return fmt.Errorf("missing public_key or secret_key")
	}

	payload := map[string]any{
		"batch": []ingestionEvent{{
			ID:        newID(),
			Type:      "trace-create",
			Timestamp: nowISO(),
			Body: map[string]any{
				"id":      newID(),
				"name":    "oat-telemetry-ping",
				"release": cfg.Release,
			},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, host+"/api/public/ingestion", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "oat-telemetry/1 (ping)")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect %s: %w", host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("authentication failed (HTTP 401) — check your public/secret keys")
	}
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("forbidden (HTTP 403) — keys may be for a different project")
	}
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
