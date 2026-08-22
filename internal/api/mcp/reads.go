package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// classify turns an error from a shared write intent into a JSON-RPC error.
//
// The judgement of "caller's fault or ours" lives in resources.ClientFault, so
// the two front doors cannot disagree about it. What differs here is only the
// vocabulary it is rendered into.
func classify(err error) error {
	if msg, isClient := resources.ClientFault(err); isClient {
		return &toolError{code: codeInvalidParams, err: fmt.Errorf("%s", msg)}
	}
	return err
}

// maxRows bounds every listing this package returns.
//
// An agent asking "what is missing" wants a list it can reason about, not a
// dump of a library. Ten thousand rows would blow past a model's context and
// be less useful than fifty — so the cap is low, and every response says
// whether it was truncated rather than silently pretending to be complete.
const maxRows = 200

// truncatable is the envelope every listing uses.
//
// `truncated` is not decoration: a list that was cut and does not say so is a
// list an agent will treat as exhaustive, and it will then confidently tell
// someone their library has fifty missing items when it has four hundred.
type truncatable struct {
	Count     int  `json:"count"`
	Truncated bool `json:"truncated"`
}

// searchContent finds works already in the library.
func (s *Server) searchContent(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Query       string `json:"query"`
		ContentType string `json:"content_type"`
		Limit       int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Query) == "" && args.ContentType == "" {
		return nil, invalidParams("give me a query or a content_type to search on")
	}
	limit := clampLimit(args.Limit)

	where := []string{"1 = 1"}
	sqlArgs := []any{}
	if q := strings.TrimSpace(args.Query); q != "" {
		// Matched against sort_title, which is the normalised form the scanner
		// records — so "the conversation" finds "The Conversation" without the
		// caller knowing how identification normalises anything.
		where = append(where, "sort_title LIKE ?")
		sqlArgs = append(sqlArgs, "%"+strings.ToLower(q)+"%")
	}
	if ct := strings.TrimSpace(args.ContentType); ct != "" {
		where = append(where, "content_type = ?")
		sqlArgs = append(sqlArgs, strings.ToLower(ct))
	}
	sqlArgs = append(sqlArgs, limit+1)

	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT id, content_type, title, year FROM works WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY sort_title, id LIMIT ?`

	rows, err := s.reader.QueryContext(ctx, stmt, sqlArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type work struct {
		WorkID      string `json:"work_id"`
		ContentType string `json:"content_type"`
		Title       string `json:"title"`
		Year        *int64 `json:"year,omitempty"`
	}
	out := struct {
		truncatable
		Works []work `json:"works"`
	}{Works: []work{}}

	for rows.Next() {
		var w work
		if err := rows.Scan(&w.WorkID, &w.ContentType, &w.Title, &w.Year); err != nil {
			return nil, err
		}
		out.Works = append(out.Works, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Works) > limit {
		out.Works = out.Works[:limit]
		out.Truncated = true
	}
	out.Count = len(out.Works)
	return out, nil
}

// wantSummary is one desired item as an agent needs it.
//
// Deliberately not the full DesiredItem: an agent reading a list of forty wants
// needs the target, the standard and the state, and a wall of timestamps and
// identifiers costs it attention it should be spending on the answer.
type wantSummary struct {
	DesiredItemID string `json:"desired_item_id"`
	WorkID        string `json:"work_id"`
	Title         string `json:"title"`
	Profile       string `json:"quality_profile"`
	State         string `json:"state"`
	Monitor       bool   `json:"monitor"`
	Reason        string `json:"reason,omitempty"`
}

// wantQuery is the shared read behind get_missing_content and
// get_upgrade_candidates.
//
// One query with a predicate rather than two, because the two questions differ
// only in which acquisition state they select — and two near-identical joins
// would be two places to fix when the state model grows.
func (s *Server) wantQuery(ctx context.Context, predicate string, limit int) (any, error) {
	//nolint:gosec // predicate is a package-level literal, never caller input
	stmt := `
		SELECT d.id, d.work_id, w.title, q.name, d.monitor, d.reason,
		       a.phase, a.managed, a.content, a.placement
		FROM desired_items d
		JOIN works w ON w.id = d.work_id
		JOIN quality_profiles q ON q.id = d.quality_profile_id
		LEFT JOIN acquisition_state a ON a.desired_item_id = d.id
		WHERE ` + predicate + `
		ORDER BY d.created_at, d.id
		LIMIT ?`

	rows, err := s.reader.QueryContext(ctx, stmt, limit+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := struct {
		truncatable
		Wants []wantSummary `json:"wants"`
	}{Wants: []wantSummary{}}

	for rows.Next() {
		var (
			w                         wantSummary
			monitor                   int
			phase, content, placement *string
			managed                   *int64
		)
		if err := rows.Scan(&w.DesiredItemID, &w.WorkID, &w.Title, &w.Profile,
			&monitor, &w.Reason, &phase, &managed, &content, &placement); err != nil {
			return nil, err
		}
		w.Monitor = monitor == 1
		w.State = deriveState(phase, managed, content, placement)
		out.Wants = append(out.Wants, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Wants) > limit {
		out.Wants = out.Wants[:limit]
		out.Truncated = true
	}
	out.Count = len(out.Wants)
	return out, nil
}

// deriveState renders §64's name from the four stored facts (ADR-0027).
//
// Computed here rather than read from a column, because the name is not stored
// anywhere — it is a presentation of the facts, and the moment something
// branches on a stored copy the two axes have an ordinal in front of them
// again.
func deriveState(phase *string, managed *int64, content, placement *string) string {
	if phase == nil {
		// No acquisition row. Possible only for a want created before the
		// state machine existed; saying so beats inventing a state.
		return "UNKNOWN"
	}
	state := acquisition.State{
		Phase:     acquisition.Phase(*phase),
		Managed:   managed != nil && *managed == 1,
		Content:   acquisition.Satisfaction(deref(content)),
		Placement: acquisition.Satisfaction(deref(placement)),
	}
	return state.Name()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// getMissingContent lists wants whose content is not satisfied.
//
// "Not satisfied" covers both nothing held AND held-but-not-good-enough, which
// are different situations with the same answer to "is this met". §64 names
// them MISSING and AVAILABLE, and the state field tells them apart — which is
// why the summary carries it rather than a bare boolean.
func (s *Server) getMissingContent(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Limit int `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	return s.wantQuery(ctx,
		`(a.content IS NULL OR a.content <> 'satisfied')`, clampLimit(args.Limit))
}

// getUpgradeCandidates lists wants that could be improved.
//
// Monitored, holding content that satisfies, and not yet terminal. The terminal
// test is NOT applied here and the tool description says so: deciding it needs
// the scorer run against every asset of every want, which is a page of work to
// render a list. This is the same deliberate superset the JSON API's
// `upgradable=true` returns.
func (s *Server) getUpgradeCandidates(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Limit int `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	return s.wantQuery(ctx,
		`d.monitor = 1 AND a.managed = 1 AND a.content = 'satisfied'`,
		clampLimit(args.Limit))
}

// getContentSatisfaction explains ONE want.
//
// It reconciles rather than reading a cached answer — the same choice the HTTP
// endpoint makes, and for the same reason: an explanation that might be minutes
// stale is one nobody can trust while looking at a file they can see on disk.
func (s *Server) getContentSatisfaction(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DesiredItemID string `json:"desired_item_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.DesiredItemID == "" {
		return nil, invalidParams("desired_item_id is required")
	}

	result, err := s.resources.Satisfaction(ctx, args.DesiredItemID)
	if err != nil {
		return nil, classify(err)
	}
	return result, nil
}

// explainRelease scores releases against a profile and returns §63's reasons.
//
// The flagship. It writes nothing, so an agent may use it freely to answer
// "would this be accepted?" before anything is acquired — and the reasons come
// back with their stable rule codes intact rather than summarised, because the
// code is the part a person can act on.
func (s *Server) explainRelease(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		QualityProfile string `json:"quality_profile"`
		Releases       []struct {
			ID         string         `json:"id"`
			Title      string         `json:"title"`
			Attributes map[string]any `json:"attributes"`
		} `json:"releases"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.QualityProfile) == "" {
		return nil, invalidParams("quality_profile is required — name it as a person would")
	}
	if len(args.Releases) == 0 {
		return nil, invalidParams("give me at least one release to explain")
	}
	if len(args.Releases) > maxRows {
		return nil, invalidParams("%d releases is past the limit of %d",
			len(args.Releases), maxRows)
	}

	profile, err := s.profileByName(ctx, args.QualityProfile)
	if err != nil {
		return nil, err
	}

	candidates := make([]acquisition.ReleaseCandidate, 0, len(args.Releases))
	seen := map[string]bool{}
	for i, in := range args.Releases {
		id := in.ID
		if id == "" {
			// The id is the ranking's tie-break, so a missing one makes the
			// order depend on the order they were sent in.
			id = fmt.Sprintf("release-%d", i+1)
		}
		if seen[id] {
			return nil, invalidParams("two releases share the id %q", id)
		}
		seen[id] = true

		attrs, err := parseAttributes(in.Attributes)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, acquisition.ReleaseCandidate{
			ID: id, Title: in.Title, Attributes: attrs,
		})
	}

	ranked := acquisition.EvaluateAll(candidates, profile)

	type explained struct {
		ID         string               `json:"id"`
		Title      string               `json:"title,omitempty"`
		Accepted   bool                 `json:"accepted"`
		Score      int                  `json:"score"`
		Terminal   bool                 `json:"terminal"`
		Reasons    []acquisition.Reason `json:"reasons"`
		RejectedBy []acquisition.Reason `json:"rejected_by,omitempty"`
	}
	out := struct {
		QualityProfile string      `json:"quality_profile"`
		Selected       string      `json:"selected,omitempty"`
		Ranked         []explained `json:"ranked"`
	}{QualityProfile: profile.Name, Ranked: make([]explained, 0, len(ranked))}

	for _, r := range ranked {
		out.Ranked = append(out.Ranked, explained{
			ID:         r.Candidate.ID,
			Title:      r.Candidate.Title,
			Accepted:   r.Evaluation.Accepted,
			Score:      r.Evaluation.Score,
			Terminal:   r.Evaluation.Terminal,
			Reasons:    r.Evaluation.Reasons,
			RejectedBy: r.Evaluation.RejectedBy(),
		})
	}
	// Absent when nothing was acceptable. The first of a ranked list is not
	// necessarily acquirable — when everything was rejected it is merely the
	// least bad, and an agent reading ranked[0] as "the answer" would
	// recommend acquiring something the profile refuses.
	if best, ok := acquisition.Best(ranked); ok {
		out.Selected = best.Candidate.ID
	}
	return out, nil
}

// parseAttributes validates an attribute map, refusing an unknown name.
//
// An attribute nothing recognises is a typo, and ignoring it would produce an
// explanation that looks right and scored against nothing. A key left OUT is a
// different thing entirely and is honoured: §63 reports it as undetermined.
func parseAttributes(in map[string]any) (acquisition.Attributes, error) {
	out := make(acquisition.Attributes, len(in))
	for name, value := range in {
		attr := policy.Attribute(strings.ToLower(strings.TrimSpace(name)))
		kind, known := policy.KindOf(attr)
		if !known {
			return nil, invalidParams("there is no attribute called %q", name)
		}
		if value == nil {
			// An explicit null means "could not determine", the same as
			// leaving the key out. Accepting both is kinder than making an
			// agent remember which spelling Heyarr wanted.
			continue
		}
		v, err := coerce(attr, kind, value)
		if err != nil {
			return nil, err
		}
		out[attr] = v
	}
	return out, nil
}

// coerce turns a decoded JSON value into a policy operand of the right kind.
func coerce(attr policy.Attribute, kind policy.Kind, value any) (policy.Value, error) {
	switch kind {
	case policy.KindInt:
		n, ok := value.(float64)
		if !ok || n != float64(int64(n)) {
			return policy.Value{}, invalidParams(
				"%s takes a whole number, not %v", attr, value)
		}
		return policy.Num(int64(n)), nil
	case policy.KindText:
		str, ok := value.(string)
		if !ok {
			return policy.Value{}, invalidParams("%s takes a name, not %v", attr, value)
		}
		return policy.Text(str), nil
	case policy.KindFlag:
		b, ok := value.(bool)
		if !ok {
			return policy.Value{}, invalidParams("%s takes true or false, not %v", attr, value)
		}
		return policy.Flag(b), nil
	}
	return policy.Value{}, invalidParams("%s has an unknown kind", attr)
}

// profileByName loads a quality profile the way a person names it.
func (s *Server) profileByName(ctx context.Context, name string) (policy.Profile, error) {
	var p policy.Profile
	var accept, prefer, terminal string
	err := s.reader.QueryRowContext(ctx, `
		SELECT id, name, description, accept, prefer, terminal
		FROM quality_profiles WHERE name = ?`, strings.TrimSpace(name)).
		Scan(&p.ID, &p.Name, &p.Description, &accept, &prefer, &terminal)
	if err != nil {
		// Named, not found. An agent that guessed a profile name needs to know
		// that is what happened rather than that something broke.
		return policy.Profile{}, &toolError{
			code: codeInvalidParams,
			err:  fmt.Errorf("there is no quality profile called %q", name),
		}
	}
	for _, pair := range []struct {
		raw  string
		dest *[]policy.Rule
	}{{accept, &p.Accept}, {prefer, &p.Prefer}, {terminal, &p.Terminal}} {
		if err := json.Unmarshal([]byte(pair.raw), pair.dest); err != nil {
			return policy.Profile{}, err
		}
	}
	return p, nil
}

// getPeerStatus lists the peers this instance knows about.
func (s *Server) getPeerStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT id, name, site, mode, is_self FROM peers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type peer struct {
		PeerID string `json:"peer_id"`
		Name   string `json:"name"`
		Site   string `json:"site"`
		Mode   string `json:"mode"`
		IsSelf bool   `json:"is_self"`
	}
	out := struct {
		truncatable
		Peers []peer `json:"peers"`
		// Note says plainly that a fabric of one peer is a supported
		// deployment rather than a fault. An agent seeing a single peer would
		// otherwise reasonably report a replication problem that does not
		// exist — and that stayed true when the second peer arrived, because
		// most Heyarr installations are one machine and always will be.
		Note string `json:"note"`
	}{
		Peers: []peer{},
		Note: "More than one peer is supported and proven (M4), and so is exactly " +
			"one: a single peer here is a deployment choice, not a symptom. With one " +
			"peer there is nowhere for bytes to converge to, so placement is satisfied " +
			"the moment content is — get_content_satisfaction reports that case as " +
			"`unproven` rather than leaving it to be inferred from this list.",
	}
	for rows.Next() {
		var p peer
		var isSelf int
		if err := rows.Scan(&p.PeerID, &p.Name, &p.Site, &p.Mode, &isSelf); err != nil {
			return nil, err
		}
		p.IsSelf = isSelf == 1
		out.Peers = append(out.Peers, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Count = len(out.Peers)
	return out, nil
}

// getReplicaStatus reports which peers hold a blob.
func (s *Server) getReplicaStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		BlobHash string `json:"blob_hash"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.BlobHash == "" {
		return nil, invalidParams("blob_hash is required")
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT r.peer_id, p.name, r.state, r.verified_at
		FROM replicas r JOIN peers p ON p.id = r.peer_id
		WHERE r.blob_hash = ? ORDER BY p.name`, args.BlobHash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type replica struct {
		PeerID     string  `json:"peer_id"`
		PeerName   string  `json:"peer_name"`
		State      string  `json:"state"`
		VerifiedAt *string `json:"verified_at,omitempty"`
		// Counts says whether this copy counts for placement. A pending or
		// corrupt replica is bytes somewhere, and it is NOT a replica for the
		// question §56 asks — stating that per row saves an agent inferring
		// it from a state name it has to know the vocabulary of.
		Counts bool `json:"counts_for_placement"`
	}
	out := struct {
		truncatable
		BlobHash string    `json:"blob_hash"`
		Replicas []replica `json:"replicas"`
	}{BlobHash: args.BlobHash, Replicas: []replica{}}

	for rows.Next() {
		var r replica
		if err := rows.Scan(&r.PeerID, &r.PeerName, &r.State, &r.VerifiedAt); err != nil {
			return nil, err
		}
		r.Counts = r.State == "present"
		out.Replicas = append(out.Replicas, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Count = len(out.Replicas)
	return out, nil
}

// verifyBlob queues a re-read of a blob's bytes.
//
// Queues rather than runs. Re-hashing a large blob is minutes of I/O, and a
// tool call that held a connection open for it would time out somewhere in the
// middle and tell the agent nothing. The answer arrives on the job.
func (s *Server) verifyBlob(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		BlobHash string `json:"blob_hash"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.BlobHash == "" {
		return nil, invalidParams("blob_hash is required")
	}

	// Checked before queueing, so an agent that mistyped a hash finds out now
	// rather than from a job that fails minutes later somewhere it is not
	// watching.
	var known int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT count(*) FROM blobs WHERE hash = ?`, args.BlobHash).Scan(&known); err != nil {
		return nil, err
	}
	if known == 0 {
		return nil, invalidParams("the catalog has no blob %q", args.BlobHash)
	}

	job, err := s.jobs.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      integrity.VerifyJobType,
		Payload:   integrity.VerifyPayload{Hash: args.BlobHash},
		DedupeKey: "verify:" + args.BlobHash,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"blob_hash": args.BlobHash,
		"job_id":    job.ID,
		"status":    "queued",
		"note": "The verification runs as a job. Watch the job or the event stream " +
			"for the outcome; this reply only says it was accepted.",
	}, nil
}

// syncPeer queues a reconciliation cycle against one peer.
//
// The same intent as POST /api/v1/peers/{id}/reconcile, and for the same
// reason it queues rather than runs: reconciling is a job (invariant 4,
// ADR-0002), the worker that runs it may be another process, and the transfers
// the diff enqueues take as long as bytes take.
//
// This verb was DEFERRED from Milestone 3 because the peer model held exactly
// one peer and there was nothing to synchronise with. There is now.
func (s *Server) syncPeer(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Peer string `json:"peer"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Peer == "" {
		return nil, invalidParams("peer is required")
	}

	// Resolved to an id before queueing. A job carrying a NAME would diff
	// against a peer set keyed by id, match nothing, and succeed having done
	// nothing — the same trap POST /peers/{id}/reconcile resolves for.
	var (
		peerID string
		isSelf int
	)
	err := s.reader.QueryRowContext(ctx,
		`SELECT id, is_self FROM peers WHERE id = ? OR name = ?`,
		args.Peer, args.Peer).Scan(&peerID, &isSelf)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, invalidParams("there is no peer called %q", args.Peer)
	case err != nil:
		return nil, err
	case isSelf == 1:
		// Refused rather than quietly accepted. A cycle scoped to this node
		// diffs the desired set against what this node already holds and
		// enqueues nothing, so accepting it would report success for a
		// misunderstanding an agent would then repeat.
		return nil, invalidParams("%q is this node — synchronising is something a node "+
			"does with another peer, and a cycle scoped to itself would find nothing to do",
			args.Peer)
	}

	job, err := s.jobs.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      replication.ReconcilePeerJobType,
		Payload:   replication.ReconcilePeerPayload{PeerID: peerID},
		DedupeKey: replication.ScopedReconcilePeerDedupeKey(peerID),
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"peer_id": peerID,
		"job_id":  job.ID,
		"status":  "queued",
		"note": "The cycle runs as a job, and the transfers it decides on are further " +
			"jobs after that. Watch the job or the event stream; this reply only says " +
			"the cycle was accepted. get_content_satisfaction reports the placement " +
			"axis reaching `converging` and then `satisfied` as the bytes land.",
	}, nil
}

func clampLimit(n int) int {
	if n <= 0 || n > maxRows {
		return maxRows
	}
	return n
}
