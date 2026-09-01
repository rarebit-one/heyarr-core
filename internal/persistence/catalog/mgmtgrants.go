package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Follow-management grants — the storage half (ADR-0061, M12).
//
// A grant is the interim, operator-issued authorization that lifts a web-login
// session (ADR-0053) from the read floor to write, keyed on the approving device
// key the session carries. The default — no row — is read-only, so this store's
// whole job is the three writes an operator drives (grant, revoke) and the one
// read the auth path asks on a session request ("is this device authorised").
//
// It lives here beside the followed sources it exists to let a browser/TV
// manage, and its transitions emit identity.device.management_* events for the
// same reason enrolment does (invariant 7).

// ManagementGrant is one authorised approving device, as persisted.
type ManagementGrant struct {
	DeviceKey string    `json:"device_key"`
	Reason    string    `json:"reason"`
	GrantedAt time.Time `json:"granted_at"`
}

// GrantManagement authorises an approving device key for write-scoped web-login
// sessions, and emits the grant event. It is idempotent: re-granting an already
// authorised device updates its reason and re-stamps it rather than failing, so
// an operator re-issuing a grant is not an error — the authority is the same
// either way. A blank device key is refused, because a grant that authorises the
// empty string would silently authorise a malformed session principal.
func (c *Catalog) GrantManagement(ctx context.Context, deviceKey, reason string) (ManagementGrant, error) {
	if deviceKey == "" {
		return ManagementGrant{}, errors.New("catalog: a management grant needs a device key")
	}
	now := c.clock.Now().UTC()
	stamp := now.Format(timestampFormat)

	var ev events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session_management_grants (device_key, reason, granted_at)
			VALUES (?, ?, ?)
			ON CONFLICT (device_key) DO UPDATE SET reason = excluded.reason, granted_at = excluded.granted_at`,
			deviceKey, reason, stamp); err != nil {
			return fmt.Errorf("catalog: granting management: %w", err)
		}
		var err error
		ev, err = c.events.EmitTx(ctx, tx, events.TypeDeviceManagementGranted,
			"device", deviceKey, map[string]any{"device_key": deviceKey, "reason": reason})
		return err
	})
	if err != nil {
		return ManagementGrant{}, err
	}
	c.events.Publish(ev)
	return ManagementGrant{DeviceKey: deviceKey, Reason: reason, GrantedAt: now}, nil
}

// RevokeManagement removes a device's write authorization and emits the
// revocation event. It reports whether a grant existed so the API door can
// answer 404 rather than 204 for a device that was never granted — a revoke of
// nothing is not a success worth pretending to.
func (c *Catalog) RevokeManagement(ctx context.Context, deviceKey string) (bool, error) {
	var (
		existed bool
		ev      events.Event
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM session_management_grants WHERE device_key = ?`, deviceKey)
		if err != nil {
			return fmt.Errorf("catalog: revoking management: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		existed = true
		ev, err = c.events.EmitTx(ctx, tx, events.TypeDeviceManagementRevoked,
			"device", deviceKey, map[string]any{"device_key": deviceKey})
		return err
	})
	if err != nil {
		return false, err
	}
	if existed {
		c.events.Publish(ev)
	}
	return existed, nil
}

// ListManagementGrants returns every authorised device, newest grant first.
func (c *Catalog) ListManagementGrants(ctx context.Context) ([]ManagementGrant, error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT device_key, reason, granted_at FROM session_management_grants ORDER BY granted_at DESC, device_key`)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing management grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ManagementGrant{}
	for rows.Next() {
		var (
			g     ManagementGrant
			stamp string
		)
		if err := rows.Scan(&g.DeviceKey, &g.Reason, &stamp); err != nil {
			return nil, err
		}
		t, err := time.Parse(timestampFormat, stamp)
		if err != nil {
			return nil, fmt.Errorf("catalog: parsing a grant timestamp: %w", err)
		}
		g.GrantedAt = t
		out = append(out, g)
	}
	return out, rows.Err()
}

// ManagementAuthorized answers the one question the auth path asks on a
// session-authenticated request: is this approving device key authorised for
// write? A blank key is never authorised (it cannot have a row), which keeps a
// malformed session on the read floor. This satisfies httpapi.ManagementAuthorizer.
func (c *Catalog) ManagementAuthorized(ctx context.Context, deviceKey string) (bool, error) {
	if deviceKey == "" {
		return false, nil
	}
	var one int
	err := c.db.Reader().QueryRowContext(ctx,
		`SELECT 1 FROM session_management_grants WHERE device_key = ?`, deviceKey).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("catalog: reading a management grant: %w", err)
	}
	return true, nil
}
