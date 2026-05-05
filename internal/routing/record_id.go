package routing

import "github.com/google/uuid"

// NewRecordID returns a UUIDv7 string suitable for OutcomeRecord.RecordID.
//
// UUIDv7 is the right shape because:
//
//   - Globally unique without coordination across daemons or machines
//     (matters for opt-in upload aggregation later).
//   - First 48 bits are millisecond timestamp, so lexical sort = chronological
//     sort. Database-friendly even when the corpus moves into SQLite.
//   - Time-encoded prefix means downstream debugging ("show me all records
//     created in the same minute") works without a separate timestamp join.
//
// Falls back to a v4 UUID if v7 generation fails (the underlying syscall
// would have to fail, which doesn't happen in practice on POSIX). We never
// want a record drop because of a UUID generation hiccup.
func NewRecordID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
