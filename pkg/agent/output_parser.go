package agent

import (
	"bufio"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// EventType categorizes agent output events.
type EventType int

const (
	// EventPRCreated indicates the agent created a pull request.
	EventPRCreated EventType = iota
	// EventError indicates a rate limit, permission denied, or tool failure.
	EventError
	// EventTaskComplete indicates the agent finished its task.
	EventTaskComplete
	// EventStuck indicates repeated/looping output.
	EventStuck
	// EventIdle indicates no output for an extended period.
	EventIdle
	// EventTokenUsage indicates a token usage report from the agent.
	EventTokenUsage
)

// AgentEvent represents a detected event in agent output.
type AgentEvent struct {
	Type      EventType
	Message   string
	Timestamp time.Time
}

// String returns a human-readable event type name.
func (t EventType) String() string {
	switch t {
	case EventPRCreated:
		return "pr_created"
	case EventError:
		return "error"
	case EventTaskComplete:
		return "task_complete"
	case EventStuck:
		return "stuck"
	case EventIdle:
		return "idle"
	case EventTokenUsage:
		return "token_usage"
	default:
		return "unknown"
	}
}

// oatTokensPrefix is the marker emitted by oat-agent for structured token data.
const oatTokensPrefix = "[OAT_TOKENS] "

var (
	// prURLPattern matches GitHub PR URLs in output.
	prURLPattern = regexp.MustCompile(`https://github\.com/[^\s]+/pull/\d+`)

	// errorPatterns match known error conditions.
	errorPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)rate limit`),
		regexp.MustCompile(`(?i)permission denied`),
		regexp.MustCompile(`(?i)tool.*(?:failed|error)`),
		regexp.MustCompile(`(?i)api.*error`),
		regexp.MustCompile(`(?i)429\s+too many requests`),
	}

	// taskCompletePatterns match task completion signals.
	taskCompletePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)task\s+complete`),
		regexp.MustCompile(`(?i)successfully\s+merged`),
		regexp.MustCompile(`(?i)pull\s+request\s+(?:has\s+been\s+)?merged`),
	}
)

// OutputWatcher tails agent output and emits events on detected patterns.
type OutputWatcher struct {
	events chan AgentEvent
	done   chan struct{}
	once   sync.Once

	// Stuck detection: ring buffer of recent output chunks
	recentChunks []string
	chunkIdx     int
	chunkSize    int // number of slots in ring buffer

	// Idle detection
	idleTimeout  time.Duration
	lastActivity time.Time
}

// OutputWatcherOption configures an OutputWatcher.
type OutputWatcherOption func(*OutputWatcher)

// WithIdleTimeout sets the duration of no output before an idle event is emitted.
func WithIdleTimeout(d time.Duration) OutputWatcherOption {
	return func(w *OutputWatcher) {
		w.idleTimeout = d
	}
}

// WithStuckBufferSize sets the number of chunks to keep for stuck detection.
func WithStuckBufferSize(n int) OutputWatcherOption {
	return func(w *OutputWatcher) {
		w.chunkSize = n
	}
}

// NewOutputWatcher creates a watcher that reads from r and emits events.
// The watcher runs in a goroutine until the reader is closed or Stop is called.
func NewOutputWatcher(r io.Reader, opts ...OutputWatcherOption) *OutputWatcher {
	w := &OutputWatcher{
		events:       make(chan AgentEvent, 32),
		done:         make(chan struct{}),
		chunkSize:    5,
		idleTimeout:  10 * time.Minute,
		lastActivity: time.Now(),
	}
	for _, opt := range opts {
		opt(w)
	}
	w.recentChunks = make([]string, w.chunkSize)

	go w.watch(r)
	return w
}

// Events returns a channel of detected agent events.
func (w *OutputWatcher) Events() <-chan AgentEvent {
	return w.events
}

// Stop terminates the watcher.
func (w *OutputWatcher) Stop() {
	w.once.Do(func() {
		close(w.done)
	})
}

func (w *OutputWatcher) watch(r io.Reader) {
	defer close(w.events)

	// tailReader wraps r so that EOF causes a brief sleep+retry instead of
	// terminating the scanner. This lets us tail a regular file that is
	// being appended to by the agent process (via cleanLogWriter). The
	// watcher's done channel stops the retry loop.
	tr := &tailReader{r: r, done: w.done, pollInterval: 250 * time.Millisecond}

	scanner := bufio.NewScanner(tr)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	idleTicker := time.NewTicker(30 * time.Second)
	defer idleTicker.Stop()

	lineCh := make(chan string, 32)
	go func() {
		defer close(lineCh)
		for scanner.Scan() {
			select {
			case lineCh <- scanner.Text():
			case <-w.done:
				return // watcher stopped — exit to avoid goroutine leak
			}
		}
	}()

	for {
		select {
		case <-w.done:
			return
		case line, ok := <-lineCh:
			if !ok {
				return // scanner finished, all lines processed
			}
			w.lastActivity = time.Now()
			w.processLine(line)
		case <-idleTicker.C:
			if time.Since(w.lastActivity) >= w.idleTimeout {
				w.emit(EventIdle, "no output for "+w.idleTimeout.String())
				// Reset to avoid spamming
				w.lastActivity = time.Now()
			}
		}
	}
}

// tailReader wraps an io.Reader (typically a regular file) so that reads
// at EOF block and retry until either new data arrives or the done channel
// is closed.  This turns a one-shot file read into a tail -f equivalent.
type tailReader struct {
	r            io.Reader
	done         <-chan struct{}
	pollInterval time.Duration
}

func (t *tailReader) Read(p []byte) (int, error) {
	for {
		n, err := t.r.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != io.EOF {
			return 0, err
		}
		// EOF — wait and retry unless stopped
		select {
		case <-t.done:
			return 0, io.EOF
		case <-time.After(t.pollInterval):
			// retry
		}
	}
}

func (w *OutputWatcher) processLine(line string) {
	// Check for structured token usage line
	if idx := strings.Index(line, oatTokensPrefix); idx >= 0 {
		w.emit(EventTokenUsage, line[idx+len(oatTokensPrefix):])
		return // token lines don't need further pattern matching
	}

	// Check for PR URLs
	if match := prURLPattern.FindString(line); match != "" {
		w.emit(EventPRCreated, match)
	}

	// Check for errors
	for _, p := range errorPatterns {
		if p.MatchString(line) {
			w.emit(EventError, strings.TrimSpace(line))
			break
		}
	}

	// Check for task completion
	for _, p := range taskCompletePatterns {
		if p.MatchString(line) {
			w.emit(EventTaskComplete, strings.TrimSpace(line))
			break
		}
	}

	// Stuck detection: accumulate chunks and check similarity
	if len(line) >= 100 {
		w.recentChunks[w.chunkIdx%w.chunkSize] = line
		w.chunkIdx++
		if w.chunkIdx >= w.chunkSize {
			w.checkStuck()
		}
	}
}

func (w *OutputWatcher) emit(t EventType, msg string) {
	event := AgentEvent{
		Type:      t,
		Message:   msg,
		Timestamp: time.Now(),
	}
	select {
	case w.events <- event:
	case <-w.done:
	default:
		// Drop event if channel is full rather than blocking
	}
}

// checkStuck computes average Jaccard similarity of character trigrams
// across recent chunks. If average > 0.8, emits EventStuck.
func (w *OutputWatcher) checkStuck() {
	filled := w.chunkSize
	if w.chunkIdx < w.chunkSize {
		filled = w.chunkIdx
	}
	if filled < 2 {
		return
	}

	var totalSim float64
	var comparisons int

	for i := 0; i < filled; i++ {
		for j := i + 1; j < filled; j++ {
			sim := trigramJaccard(w.recentChunks[i], w.recentChunks[j])
			totalSim += sim
			comparisons++
		}
	}

	if comparisons > 0 && totalSim/float64(comparisons) > 0.8 {
		w.emit(EventStuck, "repeated output detected")
		// Reset buffer to avoid repeated alerts
		w.chunkIdx = 0
		for i := range w.recentChunks {
			w.recentChunks[i] = ""
		}
	}
}

// trigramJaccard computes Jaccard similarity of character trigrams between two strings.
func trigramJaccard(a, b string) float64 {
	if len(a) < 3 || len(b) < 3 {
		return 0
	}

	setA := charTrigrams(a)
	setB := charTrigrams(b)

	intersection := 0
	for tri := range setA {
		if setB[tri] {
			intersection++
		}
	}

	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func charTrigrams(s string) map[string]bool {
	set := make(map[string]bool)
	runes := []rune(s)
	for i := 0; i <= len(runes)-3; i++ {
		set[string(runes[i:i+3])] = true
	}
	return set
}
