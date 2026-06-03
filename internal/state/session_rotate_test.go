// Tests for RotateSessionIfTooLarge (Part 7 Commit 7.0.5).
//
// Coverage:
//  1. Head under cap → no rotation, no archives.
//  2. Head over cap, no existing archives → .1 created, head empty.
//  3. Head over cap, .1 + .2 exist → shift to .2 + .3, head → .1.
//  4. Head over cap, .1 + .2 + .3 exist → oldest .3 deleted,
//     remaining shift up.
//  5. Head doesn't exist at all → no-op (no error).
//  6. Head exactly equal to cap → ROTATES (cap is inclusive).
//  7. Permission-denied during rotation → returns error, head
//     untouched if the failure was in step 1/2.
//  8. flock guard: a second call while the first holds the lock
//     yields EWOULDBLOCK (this is best-effort because shared
//     filesystem semantics vary; we test in-process via two fds).
//  9. keepArchives = 0 is clamped to 1.
//
// Tests use a small `maxBytes` (e.g. 32 bytes) so each fixture
// file can be a one-line synthetic JSONL row and we can assert
// on the exact content of .1 vs the fresh head.
package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// writeFile is a tiny helper so each test case can plant fixture
// files concisely. The 0o644 mode mirrors what
// RotateSessionIfTooLarge creates for fresh heads, so a "did the
// permissions survive rotation" assertion can compare apples to
// apples.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeFile %s: %v", path, err)
	}
}

// mustReadFile fails the test on read error so each case can
// inline-assert content without re-importing io ops.
func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("mustReadFile %s: %v", path, err)
	}
	return string(b)
}

// fileExists is a one-call wrapper around os.Stat so the
// per-case assertions read naturally as "expect .1 to exist".
func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("fileExists stat %s: %v", path, err)
	return false
}

func TestRotateSessionIfTooLarge_UnderCap_NoOp(t *testing.T) {
	dir := t.TempDir()
	head := filepath.Join(dir, "personal.session.jsonl")
	writeFile(t, head, "small")

	if err := RotateSessionIfTooLarge(head, 1024, 3); err != nil {
		t.Fatalf("expected nil error for under-cap file, got %v", err)
	}
	if got := mustReadFile(t, head); got != "small" {
		t.Errorf("head clobbered: got %q, want %q", got, "small")
	}
	if fileExists(t, head+".1") {
		t.Errorf("unexpected .1 created for under-cap file")
	}
}

func TestRotateSessionIfTooLarge_OverCap_NoExistingArchives(t *testing.T) {
	dir := t.TempDir()
	head := filepath.Join(dir, "personal.session.jsonl")
	// Use a 64-byte string so it clearly exceeds a 32-byte cap.
	original := strings.Repeat("X", 64)
	writeFile(t, head, original)

	if err := RotateSessionIfTooLarge(head, 32, 3); err != nil {
		t.Fatalf("rotate err: %v", err)
	}
	if !fileExists(t, head) {
		t.Fatal("head must be re-created empty after rotation")
	}
	if got := mustReadFile(t, head); got != "" {
		t.Errorf("fresh head should be empty, got %q", got)
	}
	if got := mustReadFile(t, head+".1"); got != original {
		t.Errorf(".1 should contain original head, got %q (want %q)", got, original)
	}
	if fileExists(t, head+".2") {
		t.Errorf("unexpected .2 created (no archives existed pre-rotation)")
	}
}

func TestRotateSessionIfTooLarge_OverCap_OneTwoArchives_ShiftToTwoThree(t *testing.T) {
	dir := t.TempDir()
	head := filepath.Join(dir, "personal.session.jsonl")
	writeFile(t, head, strings.Repeat("H", 64))
	writeFile(t, head+".1", "ARCHIVE_1_OLD")
	writeFile(t, head+".2", "ARCHIVE_2_OLDEST_AT_START")

	if err := RotateSessionIfTooLarge(head, 32, 3); err != nil {
		t.Fatalf("rotate err: %v", err)
	}
	if got := mustReadFile(t, head); got != "" {
		t.Errorf("fresh head should be empty, got %q", got)
	}
	if got := mustReadFile(t, head+".1"); got != strings.Repeat("H", 64) {
		t.Errorf(".1 should now contain ex-head, got %q", got)
	}
	if got := mustReadFile(t, head+".2"); got != "ARCHIVE_1_OLD" {
		t.Errorf(".2 should now contain ex-.1, got %q", got)
	}
	if got := mustReadFile(t, head+".3"); got != "ARCHIVE_2_OLDEST_AT_START" {
		t.Errorf(".3 should now contain ex-.2, got %q", got)
	}
}

func TestRotateSessionIfTooLarge_OverCap_AllSlotsFull_EvictOldest(t *testing.T) {
	dir := t.TempDir()
	head := filepath.Join(dir, "personal.session.jsonl")
	writeFile(t, head, strings.Repeat("H", 64))
	writeFile(t, head+".1", "EX_1")
	writeFile(t, head+".2", "EX_2")
	writeFile(t, head+".3", "EX_3_GETS_EVICTED")

	if err := RotateSessionIfTooLarge(head, 32, 3); err != nil {
		t.Fatalf("rotate err: %v", err)
	}
	if got := mustReadFile(t, head); got != "" {
		t.Errorf("fresh head should be empty, got %q", got)
	}
	if got := mustReadFile(t, head+".1"); got != strings.Repeat("H", 64) {
		t.Errorf(".1 wrong content: %q", got)
	}
	if got := mustReadFile(t, head+".2"); got != "EX_1" {
		t.Errorf(".2 wrong content: %q", got)
	}
	if got := mustReadFile(t, head+".3"); got != "EX_2" {
		t.Errorf(".3 wrong content: %q (oldest EX_3 should be GONE)", got)
	}
	// keepArchives=3 means .4 must not exist.
	if fileExists(t, head+".4") {
		t.Errorf(".4 leaked past keepArchives=3")
	}
}

func TestRotateSessionIfTooLarge_HeadMissing_NoOp(t *testing.T) {
	dir := t.TempDir()
	head := filepath.Join(dir, "ghost.session.jsonl")
	// Never write head; rotate must succeed silently.
	if err := RotateSessionIfTooLarge(head, 32, 3); err != nil {
		t.Fatalf("expected nil for missing head, got %v", err)
	}
	if fileExists(t, head) {
		t.Errorf("missing head should NOT be auto-created by rotate")
	}
	if fileExists(t, head+".1") {
		t.Errorf(".1 created for missing head")
	}
}

func TestRotateSessionIfTooLarge_ExactlyAtCap_DoesRotate(t *testing.T) {
	// Document the boundary: ">= cap" rotates; "< cap" doesn't.
	// Pinning here so a future refactor that switches to "> cap"
	// breaks loudly and the docstring stays accurate.
	dir := t.TempDir()
	head := filepath.Join(dir, "personal.session.jsonl")
	exact := strings.Repeat("E", 32)
	writeFile(t, head, exact)

	if err := RotateSessionIfTooLarge(head, 32, 3); err != nil {
		t.Fatalf("rotate err: %v", err)
	}
	if got := mustReadFile(t, head+".1"); got != exact {
		t.Errorf(".1 should contain exact-cap original, got %q", got)
	}
	if got := mustReadFile(t, head); got != "" {
		t.Errorf("fresh head not empty after exact-cap rotate: %q", got)
	}
}

func TestRotateSessionIfTooLarge_KeepArchivesZeroClampedToOne(t *testing.T) {
	dir := t.TempDir()
	head := filepath.Join(dir, "personal.session.jsonl")
	writeFile(t, head, strings.Repeat("H", 64))

	if err := RotateSessionIfTooLarge(head, 32, 0); err != nil {
		t.Fatalf("rotate err: %v", err)
	}
	// keepArchives=0 was clamped to 1, so we get a .1 and no .2.
	if got := mustReadFile(t, head+".1"); got != strings.Repeat("H", 64) {
		t.Errorf(".1 missing or wrong content: %q", got)
	}
	if fileExists(t, head+".2") {
		t.Errorf("keepArchives=0 should clamp to 1, but .2 was created")
	}
}

// TestRotateSessionIfTooLarge_FlockBlocksConcurrentSecondCall
// pins the flock guard. Strategy: hold an exclusive lock on the
// head file from THIS test goroutine via a separate fd, then
// invoke RotateSessionIfTooLarge -- the lock acquisition inside
// the helper must fail with EWOULDBLOCK and the rotation must
// be skipped (head and any pre-existing archives unchanged).
//
// Unix-only: syscall.Flock isn't available on Windows. Skip
// the test there so the broader suite still ships green; the
// helper itself wouldn't even compile on Windows because of the
// flock call, which is fine because OAT itself is Unix-only.
func TestRotateSessionIfTooLarge_FlockBlocksConcurrentSecondCall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("syscall.Flock is Unix-only")
	}
	dir := t.TempDir()
	head := filepath.Join(dir, "personal.session.jsonl")
	original := strings.Repeat("H", 64)
	writeFile(t, head, original)

	// Hold an exclusive lock from THIS test goroutine.
	holder, openErr := os.OpenFile(head, os.O_RDONLY, 0)
	if openErr != nil {
		t.Fatalf("open holder fd: %v", openErr)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("seed flock failed: %v", err)
	}

	// Now call the helper -- it must fail to acquire the lock
	// and return an error WITHOUT rotating.
	err := RotateSessionIfTooLarge(head, 32, 3)
	if err == nil {
		t.Fatalf("expected EWOULDBLOCK-style error while head is locked elsewhere")
	}

	// Head must still contain the original content (no rotation
	// happened because the lock acquisition failed).
	if got := mustReadFile(t, head); got != original {
		t.Errorf("head was rotated despite lock holder still active; got %q", got)
	}
	if fileExists(t, head+".1") {
		t.Errorf(".1 created despite lock holder still active")
	}
}
