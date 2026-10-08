package deviceauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrKeyIsServiceRecipient is an enrolment, a re-key or a device whose X25519
// key is already an executor's live service recipient (ADR-0104). A key is a
// member's (a device's or a recovery key) or an executor's, never both: the two
// are revoked differently, and removing a service recipient deletes every wrap
// of its key, which must never be a member's wrap.
var ErrKeyIsServiceRecipient = errors.New("deviceauth: that encryption key is registered as an executor's service recipient")

// rowQuerier is what a pool and a transaction both offer for one-row reads.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// refuseServiceRecipientKey is ErrKeyIsServiceRecipient when key is a live
// service recipient, read through q (the caller's transaction, so the check
// and the write that depends on it see one state). An empty key passes.
func refuseServiceRecipientKey(ctx context.Context, q rowQuerier, key string) error {
	if key == "" {
		return nil
	}
	var id string
	err := q.QueryRowContext(ctx,
		`SELECT id FROM service_recipients WHERE recipient = ? AND revoked_at IS NULL`, key).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("deviceauth: checking service recipients: %w", err)
	}
	return fmt.Errorf("%w: %s", ErrKeyIsServiceRecipient, key)
}
