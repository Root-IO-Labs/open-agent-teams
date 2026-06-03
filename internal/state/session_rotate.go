// Per-assistant session JSONL rotation (Part 7 Commit 7.0.5).
//
// What this file does
// -------------------
// Bound the on-disk size of an assistant's session JSONL (the
// per-conversation transcript the runtime reads back into LLM
// context on every `--resume`). Without a cap, a long-lived
// assistant ends up burning huge token budget on every restart
// just to come back online — the LLM has to re-ingest the full
// transcript before it can do anything useful.
//
// Design choices (pinned in Part 7 Commit 7.0.5 plan body):
//
//   - **Spawn-time only.** The runtime holds the JSONL open and
//     appends; rotating mid-write would require the writer to
//     reopen the file, which is fragile across the JS<->Python
//     <->subprocess chain. Running the rotation BEFORE the agent
//     process starts means there is no live writer by
//     construction — zero races, zero cooperation required from
//     the runtime.
//   - **Head-only restore.** The agent reads only the head file
//     (`session.jsonl`) at `--resume` time. Archives are forensic
//     only. If a user genuinely wants the older history back, the
//     docs (PAUSE_AND_RESUME.md) tell them to rename `.1` back to
//     `session.jsonl` manually. Simpler than a Claude-Code-style
//     sidecar; we can revisit if rotation proves too aggressive.
//   - **Cap = 50 MB, keep 3 archives.** Round numbers; 50 MB of
//     JSONL is many thousands of turns and the 4× ceiling (head +
//     3 archives = 200 MB / assistant) bounds disk usage without
//     a periodic cleanup loop.
//   - **flock guard.** A process-level advisory lock on the head
//     file means a concurrent second spawn (defensive — the
//     daemon's state mutex already serialises spawns, but Part
//     7.1's per-agent mutex isn't in flight yet) can't double-
//     rotate. The non-blocking acquire keeps the spawn path from
//     stalling if some other tooling has the file open.
//
// Errors are returned to the caller, which is expected to log
// them as warnings and proceed without rotation. Rotation MUST
// never block agent spawn — a permission-denied rename here is
// strictly less harmful than a botched restart.
package state

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// DefaultSessionRotateMaxBytes is the on-disk size threshold
// (in bytes) at which the head file gets rotated. 50 MiB.
// Exported so callers can pass it explicitly instead of magic-
// numbering each call site; tests use a much smaller value.
const DefaultSessionRotateMaxBytes int64 = 50 * 1024 * 1024

// DefaultSessionRotateKeepArchives is the number of historical
// archives kept after rotation. Files beyond this index are
// deleted. Keeping 3 means an assistant's worst-case disk
// footprint is roughly 4×DefaultSessionRotateMaxBytes (200 MB)
// before the next rotation prunes the oldest archive.
const DefaultSessionRotateKeepArchives = 3

// RotateSessionIfTooLarge is the public entry point for
// spawn-time JSONL rotation. Safe to call when:
//
//   - The head file does not exist (treated as size 0; no-op).
//   - The head file exists and is under maxBytes (no-op).
//   - The head file exists and is at or over maxBytes (rotate).
//
// Rotation, when triggered, does the following atomically with
// respect to a flock on the head file:
//
//  1. Delete `<headPath>.<keepArchives>` if it exists (oldest
//     slot is overwritten).
//  2. Shift `<headPath>.<i>` → `<headPath>.<i+1>` for
//     `i = keepArchives-1` down to `1`.
//  3. Rename `<headPath>` → `<headPath>.1`.
//  4. Create a fresh empty `<headPath>` so the runtime sees
//     a writable file on startup (avoids a "file not found"
//     branch in any consumer that doesn't lazy-create).
//
// On any rename/delete failure, return the underlying error so
// the caller can decide whether to surface it. The function
// never panics; on a corrupt FS the worst case is "no rotation,
// caller logs error" which mirrors the documented fallback.
//
// `keepArchives` is clamped to >= 1 because a value of 0 would
// mean "rotate AND delete the just-rotated content," which the
// caller almost certainly didn't intend. Pass an explicit value
// (or DefaultSessionRotateKeepArchives) to be safe.
func RotateSessionIfTooLarge(headPath string, maxBytes int64, keepArchives int) error {
	if keepArchives < 1 {
		keepArchives = 1
	}
	info, statErr := os.Stat(headPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("session-rotate: stat %s: %w", headPath, statErr)
	}
	if info.Size() < maxBytes {
		return nil
	}

	// Acquire an exclusive non-blocking advisory lock on the head
	// file. If another spawn is mid-rotation we yield rather than
	// double-rotate -- the daemon's caller logs the EWOULDBLOCK
	// error as a warning and proceeds without rotation (the second
	// spawn has a stale view of the head size; the first spawn
	// will have done the right thing). Lock release happens via
	// defer close, which is automatic on syscall.Flock-held fds.
	lockFile, lockErr := os.OpenFile(headPath, os.O_RDONLY, 0)
	if lockErr != nil {
		return fmt.Errorf("session-rotate: open %s for lock: %w", headPath, lockErr)
	}
	defer func() { _ = lockFile.Close() }()
	if flockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); flockErr != nil {
		// EWOULDBLOCK / EAGAIN: another spawn is mid-rotation.
		// Surface the error so the caller logs "skipping rotation,
		// another spawn holds the lock" and moves on. Anything
		// else (permission etc.) is also surfaced for the same
		// log+ignore treatment.
		return fmt.Errorf("session-rotate: flock %s: %w", headPath, flockErr)
	}

	// Re-stat AFTER acquiring the lock. The pre-lock stat was a
	// fast-path check; the post-lock stat is the source of truth
	// because the file COULD have been rotated between the two by
	// another spawn (if the lock acquisition raced a release).
	info2, stat2Err := os.Stat(headPath)
	if stat2Err != nil {
		// Vanished between stat and lock-then-stat. Race-tolerant:
		// nothing to rotate, success.
		if errors.Is(stat2Err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("session-rotate: re-stat %s: %w", headPath, stat2Err)
	}
	if info2.Size() < maxBytes {
		// Already rotated by another spawn. Nothing to do.
		return nil
	}

	// Step 1: delete the oldest archive if it exists.
	oldest := fmt.Sprintf("%s.%d", headPath, keepArchives)
	if rmErr := os.Remove(oldest); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return fmt.Errorf("session-rotate: delete oldest %s: %w", oldest, rmErr)
	}

	// Step 2: shift archives up (.k-1 -> .k, ..., .1 -> .2).
	// Walk in reverse so we don't clobber a slot before it has
	// moved.
	for i := keepArchives - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", headPath, i)
		dst := fmt.Sprintf("%s.%d", headPath, i+1)
		if renErr := os.Rename(src, dst); renErr != nil {
			if errors.Is(renErr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("session-rotate: shift %s -> %s: %w", src, dst, renErr)
		}
	}

	// Step 3: rename head -> .1.
	headDotOne := fmt.Sprintf("%s.1", headPath)
	if renErr := os.Rename(headPath, headDotOne); renErr != nil {
		return fmt.Errorf("session-rotate: rename head %s -> %s: %w", headPath, headDotOne, renErr)
	}

	// Step 4: create a fresh empty head so consumers that don't
	// lazy-create see a writable file. Mode 0644 mirrors the
	// default os.Create umask; the runtime is expected to honor
	// its own umask anyway when it appends. Use O_CREATE|O_TRUNC
	// not just Create so we get a deterministic empty file even
	// if a leftover sibling somehow exists at the same path.
	headFile, createErr := os.OpenFile(headPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if createErr != nil {
		return fmt.Errorf("session-rotate: create fresh head %s: %w", headPath, createErr)
	}
	_ = headFile.Close()
	return nil
}
