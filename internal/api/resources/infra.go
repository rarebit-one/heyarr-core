package resources

import (
	"database/sql"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// ---------------------------------------------------------------------------
// Peers
// ---------------------------------------------------------------------------

const peerColumns = `id, name, site, mode, endpoint, public_key, is_self, created_at, enrolled_at`

func scanPeer(row interface{ Scan(...any) error }) (Peer, error) {
	var p Peer
	var endpoint sql.NullString
	var publicKey []byte
	var isSelf int
	var created string
	var enrolled sql.NullString
	if err := row.Scan(&p.ID, &p.Name, &p.Site, &p.Mode, &endpoint, &publicKey, &isSelf,
		&created, &enrolled); err != nil {
		return Peer{}, err
	}
	p.Endpoint = nullString(endpoint)
	// Rendered, not raw. The column is a BLOB and JSON would base64 it into
	// something an operator cannot compare by eye with what the other site
	// shows them; the prefix says which algorithm produced it (ADR-0012).
	if len(publicKey) > 0 {
		rendered := identity.FormatPublicKey(publicKey)
		p.PublicKey = &rendered
	}
	p.IsSelf = isSelf == 1
	p.CreatedAt = parseTime(created)
	// A row written before 00020 has no enrolment time of its own. Falling
	// back to created_at is right rather than lenient: for a peer that predates
	// the column, the row appearing IS when it was admitted.
	p.EnrolledAt = p.CreatedAt
	if enrolled.Valid {
		p.EnrolledAt = parseTime(enrolled.String)
	}
	return p, nil
}

// listPeers pages by name, which is unique.
//
// There is exactly one peer in Milestone 1 and this is a paginated collection
// anyway (ADR-0010): a client written against a single-object response would
// have to be rewritten for Milestone 4, and every such rewrite is a client that
// silently keeps showing one peer instead.
func (a *API) listPeers(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "peers", 1)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if v, err := oneOf(r, "mode", "full", "partial", "cache", "archive", "compute"); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	} else if v != "" {
		where = append(where, "mode = ?")
		args = append(args, v)
	}
	if q.cursor != nil {
		where = append(where, "name > ?")
		args = append(args, q.cursor[0])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + peerColumns + ` FROM peers WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY name ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "peer", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var peers []Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			a.fail(w, r, "peer", err)
			return
		}
		peers = append(peers, p)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "peer", err)
		return
	}
	a.write(w, r, http.StatusOK, newPage(peers, q.limit,
		func(x Peer) []string { return []string{x.Name} }, "peers"))
}

// ---------------------------------------------------------------------------
// Replicas
// ---------------------------------------------------------------------------

const replicaColumns = `blob_hash, peer_id, state, bytes_present, verified_at, updated_at`

func scanReplica(row interface{ Scan(...any) error }) (Replica, error) {
	var rep Replica
	var verified sql.NullString
	var updated string
	if err := row.Scan(&rep.BlobHash, &rep.PeerID, &rep.State, &rep.BytesPresent,
		&verified, &updated); err != nil {
		return Replica{}, err
	}
	rep.VerifiedAt = parseNullTime(verified)
	rep.UpdatedAt = parseTime(updated)
	return rep, nil
}

// listReplicas pages by the table's own primary key, (blob_hash, peer_id),
// which is unique by construction.
func (a *API) listReplicas(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "replicas", 2)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	state, err := oneOf(r, "state", "present", "pending", "corrupt", "missing")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if state != "" {
		where = append(where, "state = ?")
		args = append(args, state)
	}
	if peer := r.URL.Query().Get("peer_id"); peer != "" {
		where = append(where, "peer_id = ?")
		args = append(args, peer)
	}
	if hash := r.URL.Query().Get("blob_hash"); hash != "" {
		where = append(where, "blob_hash = ?")
		args = append(args, hash)
	}
	if q.cursor != nil {
		where = append(where, "(blob_hash, peer_id) > (?, ?)")
		args = append(args, q.cursor[0], q.cursor[1])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + replicaColumns + ` FROM replicas WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY blob_hash ASC, peer_id ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "replica", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var replicas []Replica
	for rows.Next() {
		rep, err := scanReplica(rows)
		if err != nil {
			a.fail(w, r, "replica", err)
			return
		}
		replicas = append(replicas, rep)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "replica", err)
		return
	}
	a.write(w, r, http.StatusOK, newPage(replicas, q.limit,
		func(x Replica) []string { return []string{x.BlobHash, x.PeerID} }, "replicas"))
}
