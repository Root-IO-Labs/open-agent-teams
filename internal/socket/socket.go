package socket

import (
	"bufio"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

// Request represents a request sent to the daemon
type Request struct {
	Command string                 `json:"command"`
	Args    map[string]interface{} `json:"args,omitempty"`
}

// Response represents a response from the daemon
type Response struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Stream  bool        `json:"stream,omitempty"` // true if this is a streaming handshake
}

// ErrorResponse creates a failure response with the given error message.
// It supports printf-style formatting.
func ErrorResponse(format string, args ...interface{}) Response {
	return Response{
		Success: false,
		Error:   fmt.Sprintf(format, args...),
	}
}

// SuccessResponse creates a successful response with optional data.
func SuccessResponse(data interface{}) Response {
	return Response{
		Success: true,
		Data:    data,
	}
}

const (
	defaultConnectTimeout = 5 * time.Second
	defaultReadTimeout    = 10 * time.Second
	defaultWriteTimeout   = 5 * time.Second
)

// Client connects to the daemon via Unix socket
type Client struct {
	socketPath     string
	connectTimeout time.Duration
	readTimeout    time.Duration
	writeTimeout   time.Duration
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithConnectTimeout overrides the default connect timeout.
func WithConnectTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.connectTimeout = d }
}

// WithReadTimeout overrides the default read timeout.
func WithReadTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.readTimeout = d }
}

// WithWriteTimeout overrides the default write timeout.
func WithWriteTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.writeTimeout = d }
}

// NewClient creates a new socket client with optional configuration.
func NewClient(socketPath string, opts ...ClientOption) *Client {
	c := &Client{
		socketPath:     socketPath,
		connectTimeout: defaultConnectTimeout,
		readTimeout:    defaultReadTimeout,
		writeTimeout:   defaultWriteTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Send sends a request to the daemon and returns the response.
func (c *Client) Send(req Request) (*Response, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, c.connectTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
		return nil, fmt.Errorf("failed to set write deadline: %w", err)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(c.readTimeout)); err != nil {
		return nil, fmt.Errorf("failed to set read deadline: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return &resp, nil
}

// StreamConnect sends a stream request and returns the open connection
// for reading a continuous stream of JSON lines. The caller owns the
// connection and must close it when done.
//
// Protocol: client sends Request, server replies with
// {"success":true,"stream":true}, then streams JSON lines until done.
func (c *Client) StreamConnect(req Request) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, c.connectTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to daemon: %w", err)
	}

	if err := conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to set write deadline: %w", err)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send stream request: %w", err)
	}

	// Read handshake with a short timeout
	if err := conn.SetReadDeadline(time.Now().Add(c.readTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to set read deadline: %w", err)
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read stream handshake: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to parse stream handshake: %w", err)
	}
	if !resp.Success {
		conn.Close()
		return nil, fmt.Errorf("stream handshake failed: %s", resp.Error)
	}
	if !resp.Stream {
		conn.Close()
		return nil, fmt.Errorf("server did not acknowledge streaming")
	}

	// Clear deadline for the long-lived stream
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to clear deadline: %w", err)
	}

	return conn, nil
}

// StreamHandler handles long-lived streaming connections.
type StreamHandler interface {
	HandleStream(req Request, conn net.Conn)
}

// Server listens on a Unix socket for requests
type Server struct {
	socketPath    string
	listener      net.Listener
	handler       Handler
	streamHandler StreamHandler
}

// Handler processes requests
type Handler interface {
	Handle(req Request) Response
}

// HandlerFunc is an adapter to allow functions to be used as handlers
type HandlerFunc func(Request) Response

// Handle implements the Handler interface
func (f HandlerFunc) Handle(req Request) Response {
	return f(req)
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithStreamHandler sets the handler for streaming (long-lived) connections.
func WithStreamHandler(sh StreamHandler) ServerOption {
	return func(s *Server) { s.streamHandler = sh }
}

// NewServer creates a new socket server
func NewServer(socketPath string, handler Handler, opts ...ServerOption) *Server {
	s := &Server{
		socketPath: socketPath,
		handler:    handler,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start starts the socket server
func (s *Server) Start() error {
	// Remove stale socket file if exists
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}

	// Set permissions
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		listener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	s.listener = listener
	return nil
}

// maxConcurrentHandlers bounds the number of request-response handlers that
// can run simultaneously. When exceeded, new connections are rejected with a
// quick "busy" response instead of piling up goroutines, which prevents a
// slow/wedged handler from starving the daemon of goroutines or FDs.
//
// Streaming connections (stream_output, stream_events) are exempt from this
// cap since they are long-lived by design — they have their own write
// deadlines that eject stuck subscribers quickly.
const maxConcurrentHandlers = 50

// Serve accepts and handles connections with a bounded concurrency semaphore.
// Non-stream handlers are capped at maxConcurrentHandlers; excess connections
// get a fast-fail busy response so clients aren't stuck in uninterruptible
// kernel waits when the daemon is overloaded.
//
// Transient Accept errors (EMFILE, EINTR, temporary kernel hiccups) are
// logged and the loop continues. The accept loop only returns on
// net.ErrClosed (clean listener shutdown). This closes the "daemon is
// silently deaf" failure mode where one transient error would kill the
// accept loop but leave the daemon process alive and logging.
func (s *Server) Serve() error {
	sem := make(chan struct{}, maxConcurrentHandlers)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if stderrors.Is(err, net.ErrClosed) {
				return nil // clean shutdown
			}
			log.Printf("accept error (retrying): %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		select {
		case sem <- struct{}{}:
			go func(c net.Conn) {
				defer func() { <-sem }()
				s.handleConnection(c)
			}(conn)
		default:
			// Over capacity — fail the client fast with a bounded write so
			// this goroutine can't wedge either. A client that retries after
			// a short pause will likely succeed once inflight handlers drain.
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(1 * time.Second))
				_ = json.NewEncoder(c).Encode(Response{
					Success: false,
					Error:   "daemon busy: too many concurrent handlers, retry in a moment",
				})
			}(conn)
		}
	}
}

// Stop stops the server
func (s *Server) Stop() error {
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			return err
		}
	}

	// Remove socket file
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

const serverConnectionTimeout = 30 * time.Second

// handlerTimeout caps how long a synchronous (non-streaming) handler is
// allowed to run before we abandon it and reply to the client with an error.
// The handler goroutine continues if it's stuck on a mutex or syscall, but
// the connection goroutine is freed so the client isn't blocked and the
// semaphore slot is released. 20s is well under serverConnectionTimeout so
// the response has time to be written before the connection deadline hits.
const handlerTimeout = 20 * time.Second

// handleConnection handles a single connection with a deadline to prevent
// slow or stuck clients from tying up handler goroutines indefinitely.
// If the command starts with "stream_" and a StreamHandler is set,
// the connection is handed off for long-lived streaming (no deadline).
func (s *Server) handleConnection(conn net.Conn) {
	// Set initial deadline for reading the request
	if err := conn.SetDeadline(time.Now().Add(serverConnectionTimeout)); err != nil {
		conn.Close()
		return
	}

	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		if err != io.EOF {
			resp := Response{
				Success: false,
				Error:   fmt.Sprintf("failed to decode request: %v", err),
			}
			if err := json.NewEncoder(conn).Encode(resp); err != nil {
				log.Printf("Failed to encode error response: %v", err)
			} //nolint:errcheck
		}
		conn.Close()
		return
	}

	// Streaming commands: delegate to StreamHandler if available
	if strings.HasPrefix(req.Command, "stream_") && s.streamHandler != nil {
		// Clear deadline for long-lived connection
		if err := conn.SetDeadline(time.Time{}); err != nil {
			log.Printf("Failed to clear connection deadline: %v", err)
		} //nolint:errcheck
		// StreamHandler owns the connection — do NOT defer conn.Close()
		s.streamHandler.HandleStream(req, conn)
		return
	}

	// Normal request-response path.
	// Run the handler in a goroutine with a hard timeout. If the handler is
	// wedged on a mutex or slow syscall, we still reply within handlerTimeout
	// and free this connection goroutine. The wedged handler goroutine
	// eventually exits on its own (or leaks, but doesn't block new traffic).
	defer conn.Close()
	done := make(chan Response, 1)
	go func() {
		// Recover so a panicking handler doesn't crash the daemon. Panics are
		// reported as an error response to the client and logged for postmortem.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Handler panicked on command %q: %v", req.Command, r)
				done <- Response{Success: false, Error: fmt.Sprintf("handler panic: %v", r)}
			}
		}()
		done <- s.handler.Handle(req)
	}()

	var resp Response
	select {
	case resp = <-done:
	case <-time.After(handlerTimeout):
		log.Printf("Handler timeout on command %q after %s", req.Command, handlerTimeout)
		resp = Response{
			Success: false,
			Error:   fmt.Sprintf("handler timed out after %s", handlerTimeout),
		}
	}

	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		// Can't send error response at this point
		return
	}
}
