package socket

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClientServerCommunication(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	// Create handler
	handler := HandlerFunc(func(req Request) Response {
		if req.Command == "test" {
			return Response{
				Success: true,
				Data:    "test response",
			}
		}
		return Response{
			Success: false,
			Error:   "unknown command",
		}
	})

	// Start server
	server := NewServer(sockPath, handler)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer server.Stop()

	// Run server in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve()
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Create client and send request
	client := NewClient(sockPath)
	req := Request{
		Command: "test",
		Args: map[string]interface{}{
			"key": "value",
		},
	}

	resp, err := client.Send(req)
	if err != nil {
		t.Fatalf("Send() failed: %v", err)
	}

	if !resp.Success {
		t.Errorf("Response.Success = false, want true")
	}

	if resp.Data != "test response" {
		t.Errorf("Response.Data = %q, want %q", resp.Data, "test response")
	}

	// Stop server
	if err := server.Stop(); err != nil {
		t.Errorf("Stop() failed: %v", err)
	}

	// Check for server errors (expect closed connection error)
	select {
	case err := <-errCh:
		// Server should fail with "use of closed network connection" when stopped
		// This is expected and not an error
		_ = err
	case <-time.After(time.Second):
		// Server stopped cleanly
	}
}

func TestServerMultipleRequests(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	// Create counter handler
	counter := 0
	handler := HandlerFunc(func(req Request) Response {
		counter++
		return Response{
			Success: true,
			Data:    counter,
		}
	})

	// Start server
	server := NewServer(sockPath, handler)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer server.Stop()

	go server.Serve()
	time.Sleep(100 * time.Millisecond)

	// Send multiple requests
	client := NewClient(sockPath)
	for i := 1; i <= 5; i++ {
		req := Request{Command: "test"}
		resp, err := client.Send(req)
		if err != nil {
			t.Fatalf("Send(%d) failed: %v", i, err)
		}

		if !resp.Success {
			t.Errorf("Request %d: Success = false", i)
		}

		// Data should be float64 due to JSON unmarshaling of numbers
		if data, ok := resp.Data.(float64); !ok || int(data) != i {
			t.Errorf("Request %d: Data = %v, want %d", i, resp.Data, i)
		}
	}
}

func TestServerErrorResponse(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	// Create handler that returns error
	handler := HandlerFunc(func(req Request) Response {
		return Response{
			Success: false,
			Error:   "something went wrong",
		}
	})

	// Start server
	server := NewServer(sockPath, handler)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer server.Stop()

	go server.Serve()
	time.Sleep(100 * time.Millisecond)

	// Send request
	client := NewClient(sockPath)
	req := Request{Command: "test"}
	resp, err := client.Send(req)
	if err != nil {
		t.Fatalf("Send() failed: %v", err)
	}

	if resp.Success {
		t.Error("Response.Success = true, want false")
	}

	if resp.Error != "something went wrong" {
		t.Errorf("Response.Error = %q, want %q", resp.Error, "something went wrong")
	}
}

func TestClientConnectionFailure(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "nonexistent.sock")

	client := NewClient(sockPath)
	req := Request{Command: "test"}

	_, err := client.Send(req)
	if err == nil {
		t.Error("Send() succeeded when server not running")
	}
}

func TestServerRequestWithArgs(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	// Create handler that echoes args
	handler := HandlerFunc(func(req Request) Response {
		if name, ok := req.Args["name"].(string); ok {
			return Response{
				Success: true,
				Data:    "Hello, " + name,
			}
		}
		return Response{
			Success: false,
			Error:   "missing name",
		}
	})

	// Start server
	server := NewServer(sockPath, handler)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer server.Stop()

	go server.Serve()
	time.Sleep(100 * time.Millisecond)

	// Send request with args
	client := NewClient(sockPath)
	req := Request{
		Command: "greet",
		Args: map[string]interface{}{
			"name": "Alice",
		},
	}

	resp, err := client.Send(req)
	if err != nil {
		t.Fatalf("Send() failed: %v", err)
	}

	if !resp.Success {
		t.Errorf("Response.Success = false, want true")
	}

	if resp.Data != "Hello, Alice" {
		t.Errorf("Response.Data = %q, want %q", resp.Data, "Hello, Alice")
	}
}

func TestServerStaleSocket(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	// Create a stale socket file
	if err := os.WriteFile(sockPath, []byte{}, 0600); err != nil {
		t.Fatalf("Failed to create stale socket: %v", err)
	}

	// Server should remove stale socket and start successfully
	handler := HandlerFunc(func(req Request) Response {
		return Response{Success: true}
	})

	server := NewServer(sockPath, handler)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() failed with stale socket: %v", err)
	}
	defer server.Stop()
}

func TestServerInvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	// Create handler
	handler := HandlerFunc(func(req Request) Response {
		return Response{Success: true}
	})

	// Start server
	server := NewServer(sockPath, handler)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer server.Stop()

	go server.Serve()
	time.Sleep(100 * time.Millisecond)

	// Send invalid JSON directly
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Send invalid JSON
	_, err = conn.Write([]byte("not valid json\n"))
	if err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	// Read response - server should return error response
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("Failed to parse error response: %v", err)
	}

	if resp.Success {
		t.Error("Expected error response for invalid JSON")
	}

	if resp.Error == "" {
		t.Error("Expected error message in response")
	}
}

// blockingStreamHandler mimics the real daemon stream handlers: it sends
// the streaming handshake and then blocks for the lifetime of the
// connection (until the client disconnects), exactly like
// handleStreamOutput's for/select loop. It signals `active` once it has
// sent the handshake and entered its blocking read so the test can
// sequence stream establishment deterministically.
type blockingStreamHandler struct {
	active chan struct{}
}

func (h *blockingStreamHandler) HandleStream(req Request, conn net.Conn) {
	if err := json.NewEncoder(conn).Encode(Response{Success: true, Stream: true}); err != nil {
		conn.Close()
		return
	}
	if h.active != nil {
		h.active <- struct{}{}
	}
	// Block until the client closes the connection.
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			conn.Close()
			return
		}
	}
}

// TestStreamsExemptFromHandlerCap is a regression test for the bug where
// long-lived stream connections held a maxConcurrentHandlers semaphore
// slot for their entire lifetime, so a fleet of subscribers (one per
// chat-capable agent, fanned out by the bridge multiplexers) would
// saturate the cap and make ordinary request/response verbs fail with
// "daemon busy: too many concurrent handlers". After the fix the slot is
// released at stream hand-off, so streams no longer count against the cap.
func TestStreamsExemptFromHandlerCap(t *testing.T) {
	// Use a short temp dir (not t.TempDir(), whose path embeds this long
	// test name) so the Unix socket path stays under the ~104-char limit.
	tmpDir, err := os.MkdirTemp("", "oatsk")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	sockPath := filepath.Join(tmpDir, "t.sock")

	handler := HandlerFunc(func(req Request) Response {
		return Response{Success: true, Data: "ok"}
	})
	sh := &blockingStreamHandler{active: make(chan struct{}, 1)}
	server := NewServer(sockPath, handler, WithStreamHandler(sh))
	if err := server.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer server.Stop()
	go server.Serve()
	time.Sleep(100 * time.Millisecond)

	// Open well more than the handler cap in long-lived streams. Each one
	// blocks server-side; pre-fix the (cap+1)-th would itself be rejected
	// with the busy response because all slots are held.
	n := maxConcurrentHandlers + 10
	conns := make([]net.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	for i := 0; i < n; i++ {
		c, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial stream %d: %v", i, err)
		}
		if _, err := c.Write([]byte(`{"command":"stream_test"}` + "\n")); err != nil {
			t.Fatalf("write stream %d: %v", i, err)
		}
		var resp Response
		if err := json.NewDecoder(c).Decode(&resp); err != nil {
			t.Fatalf("stream %d handshake decode: %v", i, err)
		}
		if !resp.Success || !resp.Stream {
			t.Fatalf("stream %d did not get a streaming handshake (success=%v stream=%v error=%q); "+
				"the handler-cap semaphore is starving streams", i, resp.Success, resp.Stream, resp.Error)
		}
		conns = append(conns, c)
		<-sh.active // handler has entered its blocking loop
	}

	// With n (> cap) live streams, a normal request/response verb must
	// still succeed. Pre-fix this returned "daemon busy".
	client := NewClient(sockPath)
	resp, err := client.Send(Request{Command: "ping"})
	if err != nil {
		t.Fatalf("normal request after %d live streams failed: %v", n, err)
	}
	if !resp.Success {
		t.Fatalf("normal request after %d live streams got failure: %q", n, resp.Error)
	}
}

func TestServerStopWithNilListener(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	handler := HandlerFunc(func(req Request) Response {
		return Response{Success: true}
	})

	// Create server but don't start it
	server := NewServer(sockPath, handler)

	// Stop should not fail with nil listener
	if err := server.Stop(); err != nil {
		t.Errorf("Stop() failed with nil listener: %v", err)
	}
}

func TestClientReadTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	handler := HandlerFunc(func(req Request) Response {
		time.Sleep(2 * time.Second)
		return Response{Success: true}
	})

	server := NewServer(sockPath, handler)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer server.Stop()

	go server.Serve()
	time.Sleep(100 * time.Millisecond)

	client := NewClient(sockPath, WithReadTimeout(200*time.Millisecond))
	start := time.Now()
	_, err := client.Send(Request{Command: "test"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send() should have failed with read timeout")
	}

	if elapsed > 1*time.Second {
		t.Errorf("Send() took %v, expected timeout around 200ms", elapsed)
	}
}

func TestClientConnectTimeout(t *testing.T) {
	client := NewClient("/nonexistent/path/test.sock", WithConnectTimeout(200*time.Millisecond))
	start := time.Now()
	_, err := client.Send(Request{Command: "test"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send() should have failed with connect timeout")
	}

	if elapsed > 1*time.Second {
		t.Errorf("Send() took %v, expected failure within 1s", elapsed)
	}
}

func TestClientCustomTimeouts(t *testing.T) {
	client := NewClient("/tmp/test.sock",
		WithConnectTimeout(1*time.Second),
		WithReadTimeout(2*time.Second),
		WithWriteTimeout(3*time.Second),
	)

	if client.connectTimeout != 1*time.Second {
		t.Errorf("connectTimeout = %v, want 1s", client.connectTimeout)
	}
	if client.readTimeout != 2*time.Second {
		t.Errorf("readTimeout = %v, want 2s", client.readTimeout)
	}
	if client.writeTimeout != 3*time.Second {
		t.Errorf("writeTimeout = %v, want 3s", client.writeTimeout)
	}
}

func TestClientDefaultTimeouts(t *testing.T) {
	client := NewClient("/tmp/test.sock")

	if client.connectTimeout != defaultConnectTimeout {
		t.Errorf("connectTimeout = %v, want %v", client.connectTimeout, defaultConnectTimeout)
	}
	if client.readTimeout != defaultReadTimeout {
		t.Errorf("readTimeout = %v, want %v", client.readTimeout, defaultReadTimeout)
	}
	if client.writeTimeout != defaultWriteTimeout {
		t.Errorf("writeTimeout = %v, want %v", client.writeTimeout, defaultWriteTimeout)
	}
}

func TestServerStopRemovesSocket(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	handler := HandlerFunc(func(req Request) Response {
		return Response{Success: true}
	})

	server := NewServer(sockPath, handler)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Verify socket file exists
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		t.Fatal("Socket file should exist after Start()")
	}

	// Stop and verify socket file is removed
	if err := server.Stop(); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("Socket file should be removed after Stop()")
	}
}
