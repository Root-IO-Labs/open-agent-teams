package routing

import (
	"sort"
	"testing"
	"time"
)

func TestNewRecordID_NonEmpty(t *testing.T) {
	id := NewRecordID()
	if id == "" {
		t.Fatal("NewRecordID returned empty string")
	}
}

func TestNewRecordID_Unique(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewRecordID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate record id at iteration %d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewRecordID_LexicallySortableByTime is the critical UUIDv7 invariant:
// IDs generated later sort lexically after IDs generated earlier. This is
// what lets downstream code chronological-scan without a parallel ts index.
func TestNewRecordID_LexicallySortableByTime(t *testing.T) {
	const n = 50
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = NewRecordID()
		// Ensure we cross at least one millisecond boundary between IDs so
		// the v7 timestamp prefix actually advances.
		time.Sleep(2 * time.Millisecond)
	}

	// Already in generation order. Sort and confirm order is preserved.
	sorted := make([]string, n)
	copy(sorted, ids)
	sort.Strings(sorted)

	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("record id %d (%s) breaks chronological lexical sort: position in sorted = %d",
				i, ids[i], indexOf(sorted, ids[i]))
		}
	}
}

func indexOf(ss []string, target string) int {
	for i, s := range ss {
		if s == target {
			return i
		}
	}
	return -1
}
