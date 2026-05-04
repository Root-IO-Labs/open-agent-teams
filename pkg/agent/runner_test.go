package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// mockTerminal implements TerminalRunner for testing.
type mockTerminal struct {
	sendKeysCalls                 []sendKeysCall
	sendKeysLiteralCalls          []sendKeysCall
	sendKeysLiteralWithEnterCalls []sendKeysCall
	sendEnterCalls                []targetCall
	getPanePIDCalls               []targetCall
	startPipePaneCalls            []pipePaneCall
	stopPipePaneCalls             []targetCall

	getPanePIDReturn int
	getPanePIDError  error
	sendKeysError    error
}

type sendKeysCall struct {
	session string
	window  string
	text    string
}

type targetCall struct {
	session string
	window  string
}

type pipePaneCall struct {
	session    string
	window     string
	outputFile string
}

func (m *mockTerminal) SendKeys(ctx context.Context, session, window, text string) error {
	m.sendKeysCalls = append(m.sendKeysCalls, sendKeysCall{session, window, text})
	return m.sendKeysError
}

func (m *mockTerminal) SendKeysLiteral(ctx context.Context, session, window, text string) error {
	m.sendKeysLiteralCalls = append(m.sendKeysLiteralCalls, sendKeysCall{session, window, text})
	return m.sendKeysError
}

func (m *mockTerminal) SendKeysLiteralWithEnter(ctx context.Context, session, window, text string) error {
	m.sendKeysLiteralWithEnterCalls = append(m.sendKeysLiteralWithEnterCalls, sendKeysCall{session, window, text})
	return m.sendKeysError
}

func (m *mockTerminal) SendEnter(ctx context.Context, session, window string) error {
	m.sendEnterCalls = append(m.sendEnterCalls, targetCall{session, window})
	return nil
}

func (m *mockTerminal) GetPanePID(ctx context.Context, session, window string) (int, error) {
	m.getPanePIDCalls = append(m.getPanePIDCalls, targetCall{session, window})
	return m.getPanePIDReturn, m.getPanePIDError
}

func (m *mockTerminal) StartPipePane(ctx context.Context, session, window, outputFile string) error {
	m.startPipePaneCalls = append(m.startPipePaneCalls, pipePaneCall{session, window, outputFile})
	return nil
}

func (m *mockTerminal) StopPipePane(ctx context.Context, session, window string) error {
	m.stopPipePaneCalls = append(m.stopPipePaneCalls, targetCall{session, window})
	return nil
}

func TestMain(m *testing.M) {
	os.Setenv("OAT_TEST_MODE", "1")
	os.Exit(m.Run())
}

func TestNewRunner(t *testing.T) {
	runner := NewRunner()
	if runner == nil {
		t.Fatal("NewRunner() returned nil")
	}
	if runner.BinaryPath != "oat-agent" {
		t.Errorf("expected default BinaryPath to be 'oat-agent', got %q", runner.BinaryPath)
	}
	if runner.StartupDelay != 2*time.Second {
		t.Errorf("expected default StartupDelay to be 2s, got %v", runner.StartupDelay)
	}
	if runner.MessageDelay != 2*time.Second {
		t.Errorf("expected default MessageDelay to be 2s, got %v", runner.MessageDelay)
	}
	if !runner.SkipPermissions {
		t.Error("expected default SkipPermissions to be true")
	}
}

func TestNewRunnerWithOptions(t *testing.T) {
	terminal := &mockTerminal{}
	runner := NewRunner(
		WithBinaryPath("/custom/oat-agent"),
		WithTerminal(terminal),
		WithStartupDelay(1*time.Second),
		WithMessageDelay(2*time.Second),
		WithPermissions(false),
	)

	if runner.BinaryPath != "/custom/oat-agent" {
		t.Errorf("expected BinaryPath to be '/custom/oat-agent', got %q", runner.BinaryPath)
	}
	if runner.Terminal != terminal {
		t.Error("expected Terminal to be set")
	}
	if runner.StartupDelay != 1*time.Second {
		t.Errorf("expected StartupDelay to be 1s, got %v", runner.StartupDelay)
	}
	if runner.MessageDelay != 2*time.Second {
		t.Errorf("expected MessageDelay to be 2s, got %v", runner.MessageDelay)
	}
	if runner.SkipPermissions {
		t.Error("expected SkipPermissions to be false")
	}
}

func TestStart(t *testing.T) {
	ctx := context.Background()
	terminal := &mockTerminal{
		getPanePIDReturn: 12345,
	}

	runner := NewRunner(
		WithTerminal(terminal),
		WithBinaryPath("/path/to/oat-agent"),
		WithStartupDelay(0),
	)

	result, err := runner.Start(ctx, "my-session", "my-window", Config{})

	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if result.SessionID == "" {
		t.Error("expected SessionID to be generated")
	}

	if result.PID != 12345 {
		t.Errorf("expected PID to be 12345, got %d", result.PID)
	}

	if len(terminal.sendKeysCalls) != 1 {
		t.Fatalf("expected 1 SendKeys call (command only), got %d", len(terminal.sendKeysCalls))
	}

	call := terminal.sendKeysCalls[0]
	if call.session != "my-session" {
		t.Errorf("expected session 'my-session', got %q", call.session)
	}
	if call.window != "my-window" {
		t.Errorf("expected window 'my-window', got %q", call.window)
	}

	if !strings.Contains(call.text, "/path/to/oat-agent") {
		t.Errorf("expected command to contain binary path, got %q", call.text)
	}
	if !strings.Contains(call.text, "--auto-approve") {
		t.Errorf("expected command to contain --auto-approve, got %q", call.text)
	}
	if !strings.Contains(call.text, "--auto-approve") {
		t.Errorf("expected command to contain --auto-approve, got %q", call.text)
	}
}

func TestStartWithMOTD(t *testing.T) {
	ctx := context.Background()
	terminal := &mockTerminal{
		getPanePIDReturn: 12345,
	}

	runner := NewRunner(
		WithTerminal(terminal),
		WithBinaryPath("/path/to/oat-agent"),
		WithStartupDelay(0),
	)

	result, err := runner.Start(ctx, "my-session", "my-window", Config{
		MOTD: "Welcome to the agent session!",
	})

	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if result.SessionID == "" {
		t.Error("expected SessionID to be generated")
	}

	if len(terminal.sendKeysCalls) != 2 {
		t.Fatalf("expected 2 SendKeys calls (MOTD + command), got %d", len(terminal.sendKeysCalls))
	}

	motdCall := terminal.sendKeysCalls[0]
	if !strings.Contains(motdCall.text, "Welcome to the agent session!") {
		t.Errorf("expected MOTD to contain message, got %q", motdCall.text)
	}

	cmdCall := terminal.sendKeysCalls[1]
	if !strings.Contains(cmdCall.text, "/path/to/oat-agent") {
		t.Errorf("expected command to contain binary path, got %q", cmdCall.text)
	}
}

func TestStartWithCustomSessionID(t *testing.T) {
	ctx := context.Background()
	terminal := &mockTerminal{
		getPanePIDReturn: 12345,
	}

	runner := NewRunner(
		WithTerminal(terminal),
		WithStartupDelay(0),
	)

	result, err := runner.Start(ctx, "session", "window", Config{
		SessionID: "my-custom-session-id",
	})

	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if result.SessionID != "my-custom-session-id" {
		t.Errorf("expected SessionID to be 'my-custom-session-id', got %q", result.SessionID)
	}

	if len(terminal.sendKeysCalls) < 1 {
		t.Fatalf("expected at least 1 SendKeys call, got %d", len(terminal.sendKeysCalls))
	}
	if !strings.Contains(terminal.sendKeysCalls[0].text, "--auto-approve") {
		t.Errorf("expected command to contain --auto-approve, got %q", terminal.sendKeysCalls[0].text)
	}
}

func TestStartWithOutputCapture(t *testing.T) {
	ctx := context.Background()
	terminal := &mockTerminal{
		getPanePIDReturn: 12345,
	}

	runner := NewRunner(
		WithTerminal(terminal),
		WithStartupDelay(0),
	)

	_, err := runner.Start(ctx, "session", "window", Config{
		OutputFile: "/tmp/output.log",
	})

	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if len(terminal.startPipePaneCalls) != 1 {
		t.Fatalf("expected 1 StartPipePane call, got %d", len(terminal.startPipePaneCalls))
	}

	call := terminal.startPipePaneCalls[0]
	if call.outputFile != "/tmp/output.log" {
		t.Errorf("expected outputFile to be '/tmp/output.log', got %q", call.outputFile)
	}
}

func TestStartWithInitialMessage(t *testing.T) {
	ctx := context.Background()
	terminal := &mockTerminal{
		getPanePIDReturn: 12345,
	}

	runner := NewRunner(
		WithTerminal(terminal),
		WithStartupDelay(0),
		WithMessageDelay(0),
	)

	_, err := runner.Start(ctx, "session", "window", Config{
		InitialMessage: "Hello, agent!",
	})

	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if len(terminal.sendKeysLiteralWithEnterCalls) != 1 {
		t.Fatalf("expected 1 SendKeysLiteralWithEnter call, got %d", len(terminal.sendKeysLiteralWithEnterCalls))
	}

	if terminal.sendKeysLiteralWithEnterCalls[0].text != "Hello, agent!" {
		t.Errorf("expected initial message 'Hello, agent!', got %q", terminal.sendKeysLiteralWithEnterCalls[0].text)
	}
}

func TestStartNoTerminal(t *testing.T) {
	ctx := context.Background()
	runner := NewRunner()

	_, err := runner.Start(ctx, "session", "window", Config{})
	if err == nil {
		t.Error("expected error when terminal not configured")
	}
	if !strings.Contains(err.Error(), "terminal runner not configured") {
		t.Errorf("expected 'terminal runner not configured' error, got %q", err.Error())
	}
}

func TestStartSendKeysError(t *testing.T) {
	ctx := context.Background()
	terminal := &mockTerminal{
		sendKeysError: errors.New("send keys failed"),
	}

	runner := NewRunner(
		WithTerminal(terminal),
		WithStartupDelay(0),
	)

	_, err := runner.Start(ctx, "session", "window", Config{})
	if err == nil {
		t.Error("expected error when SendKeys fails")
	}
	if !strings.Contains(err.Error(), "send keys failed") {
		t.Errorf("expected 'send keys failed' error, got %q", err.Error())
	}
}

func TestStartGetPIDError(t *testing.T) {
	ctx := context.Background()
	terminal := &mockTerminal{
		getPanePIDError: errors.New("get PID failed"),
	}

	runner := NewRunner(
		WithTerminal(terminal),
		WithStartupDelay(0),
	)

	_, err := runner.Start(ctx, "session", "window", Config{})
	if err == nil {
		t.Error("expected error when GetPanePID fails")
	}
	if !strings.Contains(err.Error(), "get PID failed") {
		t.Errorf("expected 'get PID failed' error, got %q", err.Error())
	}
}

func TestStartContextCancellation(t *testing.T) {
	terminal := &mockTerminal{
		getPanePIDReturn: 12345,
	}

	runner := NewRunner(
		WithTerminal(terminal),
		WithStartupDelay(100*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runner.Start(ctx, "session", "window", Config{})
	if err == nil {
		t.Error("expected error when context is cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got %v", err)
	}
}

func TestSendMessage(t *testing.T) {
	ctx := context.Background()
	terminal := &mockTerminal{}

	runner := NewRunner(WithTerminal(terminal))

	err := runner.SendMessage(ctx, "session", "window", "Hello, agent!")
	if err != nil {
		t.Fatalf("SendMessage() failed: %v", err)
	}

	if len(terminal.sendKeysLiteralWithEnterCalls) != 1 {
		t.Fatalf("expected 1 SendKeysLiteralWithEnter call, got %d", len(terminal.sendKeysLiteralWithEnterCalls))
	}

	call := terminal.sendKeysLiteralWithEnterCalls[0]
	if call.text != "Hello, agent!" {
		t.Errorf("expected message 'Hello, agent!', got %q", call.text)
	}
}

func TestSendMessageMultiline(t *testing.T) {
	ctx := context.Background()
	terminal := &mockTerminal{}

	runner := NewRunner(WithTerminal(terminal))

	multilineMsg := "Line 1\nLine 2\nLine 3"
	err := runner.SendMessage(ctx, "session", "window", multilineMsg)
	if err != nil {
		t.Fatalf("SendMessage() failed: %v", err)
	}

	if terminal.sendKeysLiteralWithEnterCalls[0].text != multilineMsg {
		t.Errorf("expected multiline message preserved, got %q", terminal.sendKeysLiteralWithEnterCalls[0].text)
	}
}

func TestSendMessageNoTerminal(t *testing.T) {
	ctx := context.Background()
	runner := NewRunner()

	err := runner.SendMessage(ctx, "session", "window", "Hello")
	if err == nil {
		t.Error("expected error when terminal not configured")
	}
}

func TestGenerateSessionID(t *testing.T) {
	id1, err := GenerateSessionID()
	if err != nil {
		t.Fatalf("GenerateSessionID() failed: %v", err)
	}

	parts := strings.Split(id1, "-")
	if len(parts) != 5 {
		t.Errorf("expected 5 parts in UUID, got %d", len(parts))
	}

	id2, err := GenerateSessionID()
	if err != nil {
		t.Fatalf("GenerateSessionID() failed: %v", err)
	}

	if id1 == id2 {
		t.Error("expected different session IDs for each call")
	}
}

func TestBuildCommand(t *testing.T) {
	runner := NewRunner(
		WithBinaryPath("/path/to/oat-agent"),
		WithPermissions(true),
	)

	tests := []struct {
		name     string
		config   Config
		contains []string
		excludes []string
	}{
		{
			name: "basic",
			config: Config{
				SessionID: "test-session",
			},
			contains: []string{
				"/path/to/oat-agent",
				"--auto-approve",
			},
		},
		{
			name: "with workdir",
			config: Config{
				SessionID: "test-session",
				WorkDir:   "/path/to/workdir",
			},
			contains: []string{
				"cd \"/path/to/workdir\" &&",
				"/path/to/oat-agent",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := runner.buildCommand(tc.config.SessionID, tc.config)

			for _, s := range tc.contains {
				if !strings.Contains(cmd, s) {
					t.Errorf("expected command to contain %q, got %q", s, cmd)
				}
			}

			for _, s := range tc.excludes {
				if strings.Contains(cmd, s) {
					t.Errorf("expected command not to contain %q, got %q", s, cmd)
				}
			}
		})
	}
}

func TestBuildCommandWithoutSkipPermissions(t *testing.T) {
	runner := NewRunner(
		WithBinaryPath("oat-agent"),
		WithPermissions(false),
	)

	cmd := runner.buildCommand("session-id", Config{})

	if strings.Contains(cmd, "--auto-approve") {
		t.Error("expected command not to contain --auto-approve when disabled")
	}
}

func TestBuildCommandWithResume(t *testing.T) {
	runner := NewRunner(WithBinaryPath("oat-agent"))

	cmd := runner.buildCommand("test-session-id", Config{})
	if strings.Contains(cmd, "--resume") {
		t.Error("expected command not to contain --resume when Resume=false")
	}

	cmd = runner.buildCommand("test-session-id", Config{Resume: true})
	if !strings.Contains(cmd, "--resume test-session-id") {
		t.Errorf("expected command to contain --resume, got %q", cmd)
	}
	if !strings.Contains(cmd, "--auto-approve") {
		t.Error("expected command to contain --auto-approve")
	}
}

func TestBuildCommandWithModel(t *testing.T) {
	runner := NewRunner(WithBinaryPath("oat-agent"), WithPermissions(true))

	// Without model: -M should not appear
	cmd := runner.buildCommand("test-session", Config{})
	if strings.Contains(cmd, "-M ") {
		t.Errorf("expected no -M flag when Model is empty, got %q", cmd)
	}

	// With model: -M should appear with the correct value
	cmd = runner.buildCommand("test-session", Config{Model: "claude-sonnet-4-5"})
	if !strings.Contains(cmd, "-M claude-sonnet-4-5") {
		t.Errorf("expected -M claude-sonnet-4-5, got %q", cmd)
	}

	// With provider-prefixed model
	cmd = runner.buildCommand("test-session", Config{Model: "anthropic:claude-sonnet-4-5"})
	if !strings.Contains(cmd, "-M anthropic:claude-sonnet-4-5") {
		t.Errorf("expected -M anthropic:claude-sonnet-4-5, got %q", cmd)
	}
}

func TestBuildCommandWithModelParams(t *testing.T) {
	runner := NewRunner(WithBinaryPath("oat-agent"), WithPermissions(true))

	// Without model params: --model-params should not appear
	cmd := runner.buildCommand("test-session", Config{})
	if strings.Contains(cmd, "--model-params") {
		t.Errorf("expected no --model-params flag when ModelParams is empty, got %q", cmd)
	}

	// With model params: --model-params should appear with the correct value
	cmd = runner.buildCommand("test-session", Config{ModelParams: `'{"max_tokens":32000}'`})
	if !strings.Contains(cmd, `--model-params '{"max_tokens":32000}'`) {
		t.Errorf("expected --model-params with JSON, got %q", cmd)
	}
}

func TestResolveBinaryPath(t *testing.T) {
	path := ResolveBinaryPath()
	if path == "" {
		t.Error("ResolveBinaryPath() returned empty string")
	}
}

func TestIsBinaryAvailable(t *testing.T) {
	runner := NewRunner(WithBinaryPath("echo"))
	if !runner.IsBinaryAvailable() {
		t.Error("IsBinaryAvailable() should return true for 'echo'")
	}

	runner = NewRunner(WithBinaryPath("/nonexistent/binary/path"))
	if runner.IsBinaryAvailable() {
		t.Error("IsBinaryAvailable() should return false for nonexistent binary")
	}
}
