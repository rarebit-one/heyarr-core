package sqlite

import "time"

// TimestampLayout is how a timestamp that SQL compares or sorts is stored: a
// FIXED-WIDTH RFC 3339 layout in UTC, and the width is the whole reason it
// exists.
//
// time.RFC3339Nano TRIMS trailing zeros from the fractional second: 12:00:00
// UTC formats as "…T12:00:00Z", 100ms later as "…T12:00:00.1Z" and 150ms later
// as "…T12:00:00.15Z". SQLite compares TEXT byte by byte, and 'Z' (0x5A) sorts
// after both '.' (0x2E) and every digit — so under RFC3339Nano a LATER instant
// compares as the EARLIER string whenever the two fall in the same second
// (".1Z" > ".15Z"). Every `run_after <= ?`, `expires_at > ?` and
// `ORDER BY …_at` over such values is then wrong inside that second.
//
// Padding the fraction to a fixed nine digits, and always rendering UTC as
// "Z", makes lexicographic order and chronological order the same order
// again. It is still RFC 3339 (ADR-0017) and still parses as RFC3339Nano, so a
// reader — including an older binary — does not need to know.
const TimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTimestamp renders t for storage in a column SQL compares or sorts.
func FormatTimestamp(t time.Time) string { return t.UTC().Format(TimestampLayout) }

// ParseTimestamp reads a stored timestamp in either the fixed-width layout or
// the trimmed RFC3339Nano one that rows written before it still hold: Go's
// parser accepts any number of fractional digits, including none, for both.
func ParseTimestamp(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
