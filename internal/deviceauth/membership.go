package deviceauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/voidbind-go/rp"
)

// ErrMalformedOp is a membership op that does not parse, does not verify under
// its own signer, or names a different identity than the one it is recorded
// for. Recording it is refused as a whole: the op log is a set of structurally
// valid ops (voidbind-go ADR-0007 rule 1), and the callers that feed it —
// rp.Verifier and the /membership route — evaluate first and hand over only
// what evaluation accepted, so reaching this is a caller bug, not a client's.
var ErrMalformedOp = errors.New("deviceauth: malformed membership op")

// Membership adapts the Store to voidbind-go's rp.Membership for one request:
// the op log the relying-party verifier evaluates an identity over and records
// what it learns into (ADR-0068). The interface has no context parameter, so
// the adapter carries the request's; the Store methods below are the real
// implementation and take it explicitly.
func (s *Store) Membership(ctx context.Context) rp.Membership {
	return membership{s: s, ctx: ctx}
}

type membership struct {
	s   *Store
	ctx context.Context
}

func (m membership) Ops(usr string) ([]string, error) { return m.s.Ops(m.ctx, usr) }

func (m membership) Record(usr string, ops []string) error {
	return m.s.RecordOps(m.ctx, usr, ops)
}

// Ops returns every op token recorded for an identity, oldest-issued first.
// An identity with no ops — or one this node has not pinned — is nil, not an
// error: rp evaluates "nothing recorded plus whatever the device presents",
// and the pin itself is checked by the verifier, not here.
func (s *Store) Ops(ctx context.Context, usr string) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT m.token FROM membership_ops m
		 JOIN user_identities u ON u.id = m.user_id
		 WHERE u.public_key = ? ORDER BY m.iat, m.op_hash`, usr)
	if err != nil {
		return nil, fmt.Errorf("deviceauth: reading membership ops: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var tok string
		if err := rows.Scan(&tok); err != nil {
			return nil, fmt.Errorf("deviceauth: reading membership op: %w", err)
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// RecordOps appends ops to an identity's log, idempotently by op hash, and
// reconciles the device view (device_identities) with the evaluation of the
// whole log afterwards. It is the one write path into membership_ops: the
// verifier calls it with the ops a device presented that it had not seen, and
// POST /membership calls it with what a device pushed. An op that is not
// structurally valid for usr is ErrMalformedOp and nothing is written.
//
// A remove the evaluation now honours tombstones the device's row exactly as
// the admin's RevokeDevice does, and emits identity.device.removed; an add the
// evaluation now honours for a device with no row creates one, unnamed — the
// device is a member on the strength of a member's signature (ADR-0068), and
// POST /enrol is where it picks up a name. The reconciliation never clears an
// existing revoked_at: an admin's tombstone outlives the log (ADR-0067).
func (s *Store) RecordOps(ctx context.Context, usr string, ops []string) error {
	if len(ops) == 0 {
		return nil
	}
	user, err := s.LookupUser(ctx, usr)
	if err != nil {
		return err
	}
	parsed := make([]enrolment.Op, 0, len(ops))
	for _, tok := range ops {
		op, err := enrolment.VerifyOp(tok)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrMalformedOp, err.Error())
		}
		if op.User != usr {
			return fmt.Errorf("%w: op is for %s, not %s", ErrMalformedOp, op.User, usr)
		}
		parsed = append(parsed, op)
	}
	now := s.clock.Now().UTC()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("deviceauth: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var fresh []string
	for _, op := range parsed {
		prev, err := json.Marshal(op.Prev)
		if err != nil {
			return fmt.Errorf("deviceauth: encoding prev: %w", err)
		}
		if op.Prev == nil {
			prev = []byte("[]")
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO membership_ops (op_hash, user_id, dev, op, signer, prev, iat, token, received_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			op.Hash, user.ID, op.Device, string(op.Kind), op.By, string(prev),
			op.IssuedAt.UTC().Format(timeFormat), op.Token, now.Format(timeFormat))
		if err != nil {
			return fmt.Errorf("deviceauth: recording membership op: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fresh = append(fresh, op.Hash)
		}
	}
	if len(fresh) == 0 {
		return tx.Commit()
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeMembershipRecorded, "user_identity", user.ID,
		map[string]any{"public_key": usr, "ops": fresh})
	if err != nil {
		return fmt.Errorf("deviceauth: recording membership: %w", err)
	}
	published := []events.Event{ev}
	more, err := s.reconcileTx(ctx, tx, user, now)
	if err != nil {
		return err
	}
	published = append(published, more...)
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("deviceauth: committing: %w", err)
	}
	for _, ev := range published {
		s.events.Publish(ev)
	}
	return nil
}

// Reconcile re-materialises one identity's device view from its op log. It is
// what RecordOps does after every append, exposed for the verifier's fallback
// (a member whose row is missing) and for tests. Idempotent.
func (s *Store) Reconcile(ctx context.Context, usr string) error {
	user, err := s.LookupUser(ctx, usr)
	if err != nil {
		return err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("deviceauth: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	evs, err := s.reconcileTx(ctx, tx, user, s.clock.Now().UTC())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("deviceauth: committing: %w", err)
	}
	for _, ev := range evs {
		s.events.Publish(ev)
	}
	return nil
}

// reconcileTx evaluates the identity's whole log at now and brings
// device_identities into line with the view, inside the caller's transaction.
func (s *Store) reconcileTx(ctx context.Context, tx *sql.Tx, user User, now time.Time) ([]events.Event, error) {
	tokens, err := tokensTx(ctx, tx, user.ID)
	if err != nil {
		return nil, err
	}
	view, err := enrolment.Evaluate(user.PublicKey, tokens, now)
	if err != nil {
		return nil, fmt.Errorf("deviceauth: evaluating membership: %w", err)
	}

	// The view's current rows, by device key. A key already held by ANOTHER
	// user's row is left alone: device_key is UNIQUE across users, and the
	// verifier's user-match check refuses such a device rather than this
	// reconciliation reassigning it.
	existing, err := viewRowsTx(ctx, tx, user.ID)
	if err != nil {
		return nil, err
	}

	var evs []events.Event
	for dev, member := range view.Members {
		admitting, ok := view.Accepted[member.AdmittedBy]
		if !ok {
			continue // cannot happen: a member's admitting op is in the state
		}
		expires := member.ExpiresAt.UTC().Format(timeFormat)
		if r, ok := existing[dev]; ok {
			if r.cert == admitting.Token && r.enc == member.DeviceEnc && r.expires == expires {
				continue
			}
			// A renewal (any member re-adding the device) refreshes the admitting
			// op, the encryption key it binds and the expiry; revoked_at is untouched.
			if _, err := tx.ExecContext(ctx,
				`UPDATE device_identities SET cert = ?, encryption_key = ?, expires_at = ? WHERE id = ?`,
				admitting.Token, member.DeviceEnc, expires, r.id); err != nil {
				return nil, fmt.Errorf("deviceauth: refreshing device view: %w", err)
			}
			continue
		}
		var other string
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM device_identities WHERE device_key = ?`, dev).Scan(&other)
		switch {
		case err == nil:
			continue // another user's device key; refused at verification, not reassigned here
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("deviceauth: checking for an existing device: %w", err)
		}
		id := uuid.Must(uuid.NewV7()).String()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO device_identities (id, user_id, device_key, encryption_key, name, cert, enrolled_at, expires_at)
			 VALUES (?, ?, ?, ?, '', ?, ?, ?)`,
			id, user.ID, dev, member.DeviceEnc, admitting.Token, now.Format(timeFormat), expires); err != nil {
			return nil, fmt.Errorf("deviceauth: materialising device: %w", err)
		}
		ev, err := s.events.EmitTx(ctx, tx, events.TypeDeviceEnrolled, "device_identity", id,
			map[string]any{"device_key": dev, "user_id": user.ID, "name": "", "admitted_by": admitting.By})
		if err != nil {
			return nil, fmt.Errorf("deviceauth: recording enrolment: %w", err)
		}
		evs = append(evs, ev)
	}
	for dev := range view.Removed {
		r, ok := existing[dev]
		if !ok || r.revoked.Valid {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE device_identities SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
			now.Format(timeFormat), r.id); err != nil {
			return nil, fmt.Errorf("deviceauth: removing device: %w", err)
		}
		ev, err := s.events.EmitTx(ctx, tx, events.TypeDeviceRemoved, "device_identity", r.id,
			map[string]any{"device_key": dev, "user_id": user.ID})
		if err != nil {
			return nil, fmt.Errorf("deviceauth: recording removal: %w", err)
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

// viewRow is the slice of a device_identities row the reconciliation reads.
type viewRow struct {
	id, cert, enc, expires string
	revoked                sql.NullString
}

// viewRowsTx reads one user's device rows, by device key.
func viewRowsTx(ctx context.Context, tx *sql.Tx, userID string) (map[string]viewRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, device_key, cert, encryption_key, expires_at, revoked_at FROM device_identities WHERE user_id = ?`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("deviceauth: reading device view: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]viewRow{}
	for rows.Next() {
		var r viewRow
		var key string
		if err := rows.Scan(&r.id, &key, &r.cert, &r.enc, &r.expires, &r.revoked); err != nil {
			return nil, fmt.Errorf("deviceauth: reading device view: %w", err)
		}
		out[key] = r
	}
	return out, rows.Err()
}

// tokensTx reads one user's recorded op tokens inside a transaction.
func tokensTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT token FROM membership_ops WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("deviceauth: reading membership ops: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var tokens []string
	for rows.Next() {
		var tok string
		if err := rows.Scan(&tok); err != nil {
			return nil, fmt.Errorf("deviceauth: reading membership op: %w", err)
		}
		tokens = append(tokens, tok)
	}
	return tokens, rows.Err()
}

// legacyCert is a device row's cert with the identity it belongs to.
type legacyCert struct{ cert, usr string }

// unrecordedCerts lists device rows whose cert is not yet in the op log.
func (s *Store) unrecordedCerts(ctx context.Context) ([]legacyCert, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT d.cert, u.public_key FROM device_identities d
		 JOIN user_identities u ON u.id = d.user_id
		 WHERE d.cert <> '' AND NOT EXISTS (SELECT 1 FROM membership_ops m WHERE m.token = d.cert)
		 ORDER BY d.enrolled_at, d.id`)
	if err != nil {
		return nil, fmt.Errorf("deviceauth: listing legacy certs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []legacyCert
	for rows.Next() {
		var lc legacyCert
		if err := rows.Scan(&lc.cert, &lc.usr); err != nil {
			return nil, fmt.Errorf("deviceauth: reading legacy cert: %w", err)
		}
		out = append(out, lc)
	}
	return out, rows.Err()
}

// BackfillLegacyCerts records, as genesis adds, the certs of every device row
// enrolled before the op log existed (ADR-0068: a v1/v2 cert IS a genesis add,
// so nothing is reissued). Run once at startup after migration; idempotent, and
// a no-op on a node whose every cert is already in the log. It returns how many
// certs it recorded.
//
// Without it a legacy device's admission is learned only when that device next
// authenticates (the verifier records the credential it presents), which is
// correct but leaves GET /membership empty until then — and a second device,
// admitted by the first, would cite a past this node had not yet seen.
func (s *Store) BackfillLegacyCerts(ctx context.Context) (int, error) {
	certs, err := s.unrecordedCerts(ctx)
	if err != nil {
		return 0, err
	}
	byUser := map[string][]string{}
	var order []string
	for _, lc := range certs {
		if _, seen := byUser[lc.usr]; !seen {
			order = append(order, lc.usr)
		}
		byUser[lc.usr] = append(byUser[lc.usr], lc.cert)
	}
	n := 0
	for _, usr := range order {
		var ops []string
		for _, cert := range byUser[usr] {
			// A row whose cert no longer parses is left as it is: it cannot
			// authenticate under the new verifier either, and the admin's listing
			// still shows it.
			if _, err := enrolment.VerifyOp(cert); err != nil {
				continue
			}
			ops = append(ops, cert)
		}
		if err := s.RecordOps(ctx, usr, ops); err != nil {
			return n, err
		}
		n += len(ops)
	}
	return n, nil
}
