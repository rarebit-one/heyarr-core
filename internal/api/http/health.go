package httpapi

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
)

// probeTimeout bounds a readiness probe. A readiness endpoint that can hang is
// worse than one that reports not-ready: an orchestrator waiting on it will
// wait forever rather than restarting anything.
const probeTimeout = 2 * time.Second

// Check is one named readiness condition.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Detail says what is wrong in a few words. It is deliberately free of
	// paths and error text: /readyz is unauthenticated, and "the database at
	// /srv/media/heyarr.db is locked" tells a stranger more than it needs to.
	Detail string `json:"detail,omitempty"`
}

// Readiness is the GET /readyz body.
type Readiness struct {
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

// handleHealthz answers liveness: this process is running and its HTTP stack
// works. It checks nothing else on purpose — a liveness probe that fails
// because the database is busy causes a restart loop that makes the database
// busier.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz answers readiness: this process can actually serve requests.
// It stays non-200 until it can, so a load balancer does not route to a
// controller whose CAS root has not been mounted yet.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	checks := []Check{s.checkDatabase(ctx), s.checkCAS()}
	ready := true
	for _, c := range checks {
		if !c.OK {
			ready = false
		}
	}

	status := http.StatusOK
	body := Readiness{Status: "ready", Checks: checks}
	if !ready {
		status = http.StatusServiceUnavailable
		body.Status = "not ready"
	}
	s.writeJSON(w, r, status, body)
}

func (s *Server) checkDatabase(ctx context.Context) Check {
	// A ping proves the pool is alive; a trivial query proves the file is
	// readable, which is the failure that actually happens — an unmounted
	// volume leaves a healthy pool over a database that is not there.
	var one int
	if err := s.db.Reader().QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		s.log.Warn("readiness: the database is not answering", "error", err)
		return Check{Name: "database", OK: false, Detail: "unreachable"}
	}
	return Check{Name: "database", OK: true}
}

func (s *Server) checkCAS() Check {
	if s.casRoot == "" {
		return Check{Name: "cas", OK: true, Detail: "not configured"}
	}
	info, err := os.Stat(s.casRoot)
	if err != nil {
		s.log.Warn("readiness: the CAS root is not present", "error", err)
		return Check{Name: "cas", OK: false, Detail: "missing"}
	}
	if !info.IsDir() {
		return Check{Name: "cas", OK: false, Detail: "not a directory"}
	}
	// Presence is not enough. A read-only mount, a full filesystem and a
	// wrong-owner directory all stat perfectly well and then fail on the first
	// ingest — which is hours into a scan rather than at startup.
	probe, err := os.CreateTemp(s.casRoot, ".heyarr-ready-*")
	if err != nil {
		s.log.Warn("readiness: the CAS root is not writable", "error", err)
		return Check{Name: "cas", OK: false, Detail: "not writable"}
	}
	name := probe.Name()
	_ = probe.Close()
	if err := os.Remove(name); err != nil {
		s.log.Warn("readiness: could not clean up the CAS probe", "error", err)
	}
	return Check{Name: "cas", OK: true}
}

// SystemInfo is the GET /api/v1/system body: what this node is, what it is
// running, and whether the two things it depends on are working. It requires
// the `read` scope, so unlike /readyz it may name paths.
type SystemInfo struct {
	Build         buildinfo.Info `json:"build"`
	Peer          PeerInfo       `json:"peer"`
	SchemaVersion int64          `json:"schema_version"`
	Database      StorageInfo    `json:"database"`
	CAS           StorageInfo    `json:"cas"`
	AuthEnabled   bool           `json:"auth_enabled"`
}

// PeerInfo identifies this node within the instance (ADR-0010). There is
// exactly one peer in Milestone 1 and the field exists anyway, so Milestone 4
// is a protocol addition rather than an API change.
type PeerInfo struct {
	Name string `json:"name"`
	Site string `json:"site"`
}

// StorageInfo reports one dependency's location and health.
type StorageInfo struct {
	Path string `json:"path"`
	OK   bool   `json:"ok"`
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	dbCheck := s.checkDatabase(ctx)
	casCheck := s.checkCAS()

	s.writeJSON(w, r, http.StatusOK, SystemInfo{
		Build:         s.build,
		Peer:          PeerInfo{Name: s.cfg.Peer.Name, Site: s.cfg.Peer.Site},
		SchemaVersion: s.schema,
		Database:      StorageInfo{Path: s.db.Path(), OK: dbCheck.OK},
		CAS:           StorageInfo{Path: s.casRoot, OK: casCheck.OK},
		AuthEnabled:   s.cfg.HTTP.Auth.Enabled,
	})
}
