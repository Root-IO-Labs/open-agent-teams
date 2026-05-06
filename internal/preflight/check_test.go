package preflight

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// ---------------------------------------------------------------------------
// Pure parsers and helpers.
// ---------------------------------------------------------------------------

func TestParseGoVersion(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		major  int
		minor  int
		patch  int
		wantOK bool
	}{
		{"full release", "go version go1.24.2 darwin/arm64\n", 1, 24, 2, true},
		{"patchless minor", "go version go1.26 linux/amd64\n", 1, 26, 0, true},
		{"rc build", "go version go1.25rc1 darwin/arm64\n", 1, 25, 0, true},
		{"pre-release patch", "go version go1.24.2-devel darwin/arm64\n", 1, 24, 2, true},
		{"gibberish", "something unrelated\n", 0, 0, 0, false},
		{"empty", "", 0, 0, 0, false},
		{"major only", "go version go2 linux/amd64\n", 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, patch, ok := parseGoVersion(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if major != tc.major || minor != tc.minor || patch != tc.patch {
				t.Fatalf("got %d.%d.%d, want %d.%d.%d", major, minor, patch, tc.major, tc.minor, tc.patch)
			}
		})
	}
}

func TestParsePythonVersion(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		major  int
		minor  int
		wantOK bool
	}{
		{"3.11 patch", "Python 3.11.1\n", 3, 11, true},
		{"3.12 no patch", "Python 3.12\n", 3, 12, true},
		{"3.10 lowercase", "python 3.10.0\n", 3, 10, true},
		{"leading whitespace", "   Python 3.13.0  ", 3, 13, true},
		{"no python", "foo 1.2.3", 0, 0, false},
		{"empty", "", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, ok := parsePythonVersion(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if major != tc.major || minor != tc.minor {
				t.Fatalf("got %d.%d, want %d.%d", major, minor, tc.major, tc.minor)
			}
		})
	}
}

func TestAtLeast(t *testing.T) {
	cases := []struct {
		name string
		have []int
		need []int
		want bool
	}{
		{"equal", []int{1, 24, 2}, []int{1, 24, 2}, true},
		{"patch higher", []int{1, 24, 3}, []int{1, 24, 2}, true},
		{"patch lower", []int{1, 24, 1}, []int{1, 24, 2}, false},
		{"minor higher dominates lower patch", []int{1, 25, 0}, []int{1, 24, 99}, true},
		{"minor lower dominates higher patch", []int{1, 23, 99}, []int{1, 24, 0}, false},
		{"major higher", []int{2, 0, 0}, []int{1, 99, 99}, true},
		{"major lower", []int{0, 99, 99}, []int{1, 0, 0}, false},
		{"two-segment equal", []int{3, 11}, []int{3, 11}, true},
		{"two-segment below", []int{3, 10}, []int{3, 11}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := atLeast(tc.have, tc.need)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	results := []CheckResult{
		{Status: StatusOK}, {Status: StatusOK}, {Status: StatusOK},
		{Status: StatusWarn},
		{Status: StatusFail}, {Status: StatusFail},
	}
	ok, warn, fail := Summarize(results)
	if ok != 3 || warn != 1 || fail != 2 {
		t.Fatalf("Summarize = (%d, %d, %d), want (3, 1, 2)", ok, warn, fail)
	}
}

// ---------------------------------------------------------------------------
// LLM key detection.
// ---------------------------------------------------------------------------

func TestFirstSetEnv(t *testing.T) {
	const key1 = "OAT_TEST_PREFLIGHT_X"
	const key2 = "OAT_TEST_PREFLIGHT_Y"
	// Ensure clean slate
	t.Setenv(key1, "")
	t.Setenv(key2, "")

	if got := firstSetEnv([]string{key1, key2}); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	t.Setenv(key2, "some-value")
	if got := firstSetEnv([]string{key1, key2}); got != key2 {
		t.Fatalf("expected %s, got %q", key2, got)
	}
	t.Setenv(key1, "first-wins")
	if got := firstSetEnv([]string{key1, key2}); got != key1 {
		t.Fatalf("expected %s (first in list wins), got %q", key1, got)
	}
}

func TestFirstKeyInEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := strings.Join([]string{
		"# a comment",
		"",
		"ANTHROPIC_API_KEY=sk-ant-test",
		"UNRELATED_VAR=ignored",
		"# OPENAI_API_KEY=commented",
	}, "\n")
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	key, found := firstKeyInEnvFile(envPath, []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"})
	if !found {
		t.Fatalf("expected to find a key")
	}
	if key != "ANTHROPIC_API_KEY" {
		t.Fatalf("got %q, want ANTHROPIC_API_KEY", key)
	}

	// Commented lines are not matched.
	key, found = firstKeyInEnvFile(envPath, []string{"OPENAI_API_KEY"})
	if found {
		t.Fatalf("commented key should not match, got %q", key)
	}

	// Missing file returns (_, false) without error.
	key, found = firstKeyInEnvFile(filepath.Join(dir, "nope"), []string{"ANTHROPIC_API_KEY"})
	if found || key != "" {
		t.Fatalf("missing file should return empty, got (%q, %v)", key, found)
	}
}

func TestCheckLLMKey_FromEnv(t *testing.T) {
	// Clear all known keys to ensure hermetic test.
	for _, k := range KeyProviders {
		t.Setenv(k, "")
	}
	// Point paths.Root at an empty dir so the .env lookup finds nothing.
	dir := t.TempDir()
	paths := &config.Paths{Root: dir}

	r := checkLLMKey(paths)
	if r.Status != StatusFail {
		t.Fatalf("expected FAIL with no keys, got %s", r.Status)
	}

	t.Setenv("OPENAI_API_KEY", "sk-test")
	r = checkLLMKey(paths)
	if r.Status != StatusOK {
		t.Fatalf("expected OK after setting env, got %s: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "OPENAI_API_KEY") {
		t.Fatalf("message should name the key, got %q", r.Message)
	}
}

func TestCheckLLMKey_FromDotEnvFile(t *testing.T) {
	for _, k := range KeyProviders {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("ANTHROPIC_API_KEY=sk-ant-from-file\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	paths := &config.Paths{Root: dir}

	r := checkLLMKey(paths)
	if r.Status != StatusOK {
		t.Fatalf("expected OK from .env, got %s: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "ANTHROPIC_API_KEY") || !strings.Contains(r.Message, envPath) {
		t.Fatalf("message should cite key and path, got %q", r.Message)
	}
}

func TestHasAnyLLMKey(t *testing.T) {
	// Hermetic env.
	for _, k := range KeyProviders {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	paths := &config.Paths{Root: dir}

	if HasAnyLLMKey(paths) {
		t.Fatalf("expected false with no keys set")
	}

	// Key via environment.
	t.Setenv("OPENROUTER_API_KEY", "or-test")
	if !HasAnyLLMKey(paths) {
		t.Fatalf("expected true after setting env var")
	}
	t.Setenv("OPENROUTER_API_KEY", "")

	// Key via ~/.oat/.env fallback.
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("ANTHROPIC_API_KEY=sk-ant-from-file\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	if !HasAnyLLMKey(paths) {
		t.Fatalf("expected true when key is in ~/.oat/.env")
	}

}

func TestHasAnyLLMKey_NoKeyAnywhere(t *testing.T) {
	for _, k := range KeyProviders {
		t.Setenv(k, "")
	}
	// Point to an empty dir so the .env fallback finds nothing.
	empty := t.TempDir()
	paths := &config.Paths{Root: empty}
	if HasAnyLLMKey(paths) {
		t.Fatalf("expected false with no keys in env or .env")
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env")
	if !HasAnyLLMKey(paths) {
		t.Fatalf("expected true when env var is set")
	}
}

// ---------------------------------------------------------------------------
// Filesystem-touching checks.
// ---------------------------------------------------------------------------

func TestCheckOatDir_CreatesMissing(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "fresh-oat-root")
	paths := &config.Paths{Root: root}
	r := checkOatDir(paths)
	if r.Status != StatusOK {
		t.Fatalf("expected OK, got %s: %s", r.Status, r.Message)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("dir should have been created: %v", err)
	}
}

func TestCheckOatDir_NotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	base := t.TempDir()
	root := filepath.Join(base, "readonly")
	if err := os.MkdirAll(root, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// chmod back so t.TempDir() cleanup works
	defer os.Chmod(root, 0o700)
	paths := &config.Paths{Root: root}
	r := checkOatDir(paths)
	if r.Status != StatusFail {
		t.Fatalf("expected FAIL for readonly dir, got %s: %s", r.Status, r.Message)
	}
}

// ---------------------------------------------------------------------------
// Daemon check.
// ---------------------------------------------------------------------------

func TestCheckDaemon_NoPIDFile(t *testing.T) {
	base := t.TempDir()
	paths := &config.Paths{Root: base, DaemonPID: filepath.Join(base, "daemon.pid")}
	r := checkDaemon(paths)
	if r.Status != StatusWarn {
		t.Fatalf("expected WARN when no pid file, got %s: %s", r.Status, r.Message)
	}
}

func TestCheckDaemon_MalformedPID(t *testing.T) {
	base := t.TempDir()
	pidPath := filepath.Join(base, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-number"), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	paths := &config.Paths{Root: base, DaemonPID: pidPath}
	r := checkDaemon(paths)
	if r.Status != StatusWarn {
		t.Fatalf("expected WARN for malformed pid, got %s: %s", r.Status, r.Message)
	}
}

func TestCheckDaemon_Alive(t *testing.T) {
	base := t.TempDir()
	pidPath := filepath.Join(base, "daemon.pid")
	// Own pid is guaranteed alive for the duration of this test.
	ownPID := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strings.TrimSpace(itoa(ownPID))), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	paths := &config.Paths{Root: base, DaemonPID: pidPath}
	r := checkDaemon(paths)
	if r.Status != StatusOK {
		t.Fatalf("expected OK for live pid, got %s: %s", r.Status, r.Message)
	}
}

// itoa avoids pulling in strconv just for a test helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// ---------------------------------------------------------------------------
// checkGo / checkPython via hooked commands.
// ---------------------------------------------------------------------------

func TestCheckGo_Happy(t *testing.T) {
	restore := swapGoCmd(func() ([]byte, error) { return []byte("go version go1.26.1 darwin/arm64\n"), nil })
	defer restore()
	r := checkGo()
	if r.Status != StatusOK {
		t.Fatalf("expected OK, got %s: %s", r.Status, r.Message)
	}
}

func TestCheckGo_Missing(t *testing.T) {
	restore := swapGoCmd(func() ([]byte, error) { return nil, errors.New("exec: 'go' not found") })
	defer restore()
	r := checkGo()
	if r.Status != StatusFail {
		t.Fatalf("expected FAIL, got %s: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Hint, "go.dev") {
		t.Fatalf("hint should point users to install page, got %q", r.Hint)
	}
}

func TestCheckGo_TooOld(t *testing.T) {
	restore := swapGoCmd(func() ([]byte, error) { return []byte("go version go1.22.0 darwin/arm64"), nil })
	defer restore()
	r := checkGo()
	if r.Status != StatusFail {
		t.Fatalf("expected FAIL for old Go, got %s: %s", r.Status, r.Message)
	}
}

func TestCheckGo_UnparseableWarn(t *testing.T) {
	restore := swapGoCmd(func() ([]byte, error) { return []byte("weird output"), nil })
	defer restore()
	r := checkGo()
	if r.Status != StatusWarn {
		t.Fatalf("expected WARN for unparseable version, got %s: %s", r.Status, r.Message)
	}
}

func TestCheckPython_OK(t *testing.T) {
	restore := swapPythonCmd(func() (string, error) { return "Python 3.12.1\n", nil })
	defer restore()
	r := checkPython()
	if r.Status != StatusOK {
		t.Fatalf("expected OK, got %s: %s", r.Status, r.Message)
	}
}

func TestCheckPython_Missing(t *testing.T) {
	restore := swapPythonCmd(func() (string, error) { return "", errors.New("no python") })
	defer restore()
	r := checkPython()
	if r.Status != StatusFail {
		t.Fatalf("expected FAIL, got %s: %s", r.Status, r.Message)
	}
}

func TestCheckPython_TooOld(t *testing.T) {
	restore := swapPythonCmd(func() (string, error) { return "Python 3.10.14", nil })
	defer restore()
	r := checkPython()
	if r.Status != StatusFail {
		t.Fatalf("expected FAIL for 3.10, got %s: %s", r.Status, r.Message)
	}
}

// ---------------------------------------------------------------------------
// gh CLI check.
// ---------------------------------------------------------------------------

func TestCheckGhCLI_NotInstalled(t *testing.T) {
	restore := swapGh(func() ([]byte, error) { return nil, errors.New("no gh") },
		func() error { return nil })
	defer restore()
	r := checkGhCLI()
	if r.Status != StatusFail {
		t.Fatalf("expected FAIL, got %s: %s", r.Status, r.Message)
	}
}

func TestCheckGhCLI_NotAuthed(t *testing.T) {
	restore := swapGh(
		func() ([]byte, error) { return []byte("gh version 2.x"), nil },
		func() error { return errors.New("not logged in") },
	)
	defer restore()
	r := checkGhCLI()
	if r.Status != StatusFail {
		t.Fatalf("expected FAIL for unauth, got %s: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Hint, "gh auth login") {
		t.Fatalf("hint should tell user to run gh auth login, got %q", r.Hint)
	}
}

func TestCheckGhCLI_OK(t *testing.T) {
	restore := swapGh(
		func() ([]byte, error) { return []byte("gh version 2.x"), nil },
		func() error { return nil },
	)
	defer restore()
	r := checkGhCLI()
	if r.Status != StatusOK {
		t.Fatalf("expected OK, got %s: %s", r.Status, r.Message)
	}
}

// ---------------------------------------------------------------------------
// oat-agent check.
// ---------------------------------------------------------------------------

func TestCheckOatAgent_Missing(t *testing.T) {
	restore := swapOatAgent(func() (string, error) { return "", errors.New("not found") })
	defer restore()
	r := checkOatAgent()
	if r.Status != StatusFail {
		t.Fatalf("expected FAIL, got %s", r.Status)
	}
}

func TestCheckOatAgent_OK(t *testing.T) {
	restore := swapOatAgent(func() (string, error) { return "/usr/local/bin/oat-agent", nil })
	defer restore()
	r := checkOatAgent()
	if r.Status != StatusOK {
		t.Fatalf("expected OK, got %s", r.Status)
	}
	if !strings.Contains(r.Message, "oat-agent") {
		t.Fatalf("message should contain the path, got %q", r.Message)
	}
}

// ---------------------------------------------------------------------------
// End-to-end Run().
// ---------------------------------------------------------------------------

func TestRun_ReturnsAllChecksInOrder(t *testing.T) {
	// Hook every external side effect so this test is hermetic and fast.
	defer swapGoCmd(func() ([]byte, error) { return []byte("go version go1.26.0 darwin/arm64"), nil })()
	defer swapPythonCmd(func() (string, error) { return "Python 3.12.0", nil })()
	defer swapGh(func() ([]byte, error) { return []byte("gh"), nil }, func() error { return nil })()
	defer swapOatAgent(func() (string, error) { return "/x/oat-agent", nil })()

	for _, k := range KeyProviders {
		t.Setenv(k, "")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	base := t.TempDir()
	paths := &config.Paths{Root: base, DaemonPID: filepath.Join(base, "daemon.pid")}

	results := Run(paths)
	want := []string{
		"Go toolchain",
		"Python runtime",
		"GitHub CLI",
		"oat-agent binary",
		"LLM API key",
		"~/.oat directory",
		"Daemon",
	}
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for i, w := range want {
		if results[i].Name != w {
			t.Fatalf("result[%d].Name = %q, want %q", i, results[i].Name, w)
		}
	}
	// No FAILs expected in the hermetic happy path; Daemon is WARN (no pid).
	_, _, fail := Summarize(results)
	if fail != 0 {
		t.Fatalf("expected 0 FAIL in hermetic happy path, got %d: %+v", fail, results)
	}
}

// ---------------------------------------------------------------------------
// Test hook swappers (kept at the bottom so production code stays clean).
// ---------------------------------------------------------------------------

func swapGoCmd(fn func() ([]byte, error)) func() {
	old := goVersionCmd
	goVersionCmd = fn
	return func() { goVersionCmd = old }
}

func swapPythonCmd(fn func() (string, error)) func() {
	old := pythonVerCmd
	pythonVerCmd = fn
	return func() { pythonVerCmd = old }
}

func swapGh(version func() ([]byte, error), auth func() error) func() {
	oldVer := ghVersionCmd
	oldAuth := ghAuthCmd
	ghVersionCmd = version
	ghAuthCmd = auth
	return func() {
		ghVersionCmd = oldVer
		ghAuthCmd = oldAuth
	}
}

func swapOatAgent(fn func() (string, error)) func() {
	old := oatAgentLookup
	oatAgentLookup = fn
	return func() { oatAgentLookup = old }
}
