package transfer

import (
	"fmt"
	"sort"
)

// Kind is what sort of thing a source is, which decides how it is reached.
//
// The kinds are separate because the transports are: a peer is dialled over
// mTLS with a pinned key (ADR-0012), a web seed is an ordinary ranged HTTP GET
// against the same blob endpoint a client uses (§27, ADR-0013), and an external
// source is handed to a download client that talks to the outside world (§25).
type Kind string

const (
	// KindPeer is another Heyarr peer, reached over the authenticated peer
	// surface. Discovery comes from membership and never from a public tracker
	// (§26).
	KindPeer Kind = "peer"
	// KindWebSeed is a peer's ordinary blob endpoint used as a byte source
	// (§27). It is listed separately from KindPeer even when it names the same
	// machine, because it is a different contract: no piece protocol, no
	// swarm, just ranged reads that any HTTP client can perform.
	KindWebSeed Kind = "web_seed"
	// KindExternal is outside the fabric — the swarm or host a release is
	// acquired from, reached through a download client rather than directly
	// (§25). Heyarr never becomes the external transport.
	KindExternal Kind = "external"
)

// Health is what was last observed about a source's reachability.
//
// The vocabulary matches replication's, because it comes from the same peer
// rows and a second spelling would be a second thing to keep in step.
const (
	HealthReachable   = "reachable"
	HealthUnknown     = "unknown"
	HealthUnreachable = "unreachable"
)

// Urgency is how hard this transfer should try, and it is a real behavioural
// difference rather than a label.
//
// # Why it exists
//
// replication.RankSources' own comment already describes two of these without
// naming them: health.Sources drops everything unreachable, "which is right for
// a read: a client is waiting, a healthy peer three feet away holds the same
// bytes" — and then argues that replication is NOT a read, nobody is waiting,
// and excluding an unknown peer would make a fabric refuse its first transfer
// while sitting next to a peer holding every byte.
//
// Those are the same question answered differently because the CALLER is
// different. Naming it is what lets one ordering serve both.
type Urgency string

const (
	// UrgencyInteractive is somebody waiting. Sources believed to be down are
	// not attempted: a failed dial costs the person their wait, and another
	// source probably has the bytes.
	UrgencyInteractive Urgency = "interactive"
	// UrgencyBackground is durability work. EVERY usable source is attempted,
	// including ones last seen unreachable, because "known to be down" is a
	// fact with a timestamp on it and the timestamp may be older than the
	// reboot. Nobody is waiting and the queue retries.
	//
	// This is the default, and the safer one: the cost of trying a dead peer is
	// one failed connection, and the cost of skipping a live one is a
	// durability gap that reports itself as nothing to do.
	UrgencyBackground Urgency = "background"
)

// Source is somewhere the target bytes might come from.
//
// Deliberately NOT the peer row, the magnet, or the download client's struct.
// A session orders sources; how one is actually reached is the transport's
// business, and putting a dial address here would make this package care.
type Source struct {
	// ID identifies the source within its kind — a peer id, or the download
	// client's identifier for an external transfer.
	//
	// It is what the total order breaks ties on, so it must be stable across
	// runs and must not be a position in a list (ADR-0017).
	ID string
	// Kind decides which transport reaches it.
	Kind Kind
	// Health is what was last observed. Empty means unknown, which is the
	// column default and is the absence of evidence rather than evidence of
	// absence.
	Health string
}

// Session is one node's plan to obtain one blob (§24).
//
// # Why there is no Priority field
//
// §24 lists priority alongside urgency, and it is absent here on purpose.
// Nothing schedules BETWEEN sessions yet — the job queue decides what runs, by
// its own rules — so a priority field would be a value nothing reads. That is
// exactly the shape of providers.Downloader's promised-but-absent methods,
// which is the defect #225 turned out to be. It goes in when something orders
// sessions by it.
//
// Urgency is here because it changes the plan, today, in Plan.
type Session struct {
	// Target is the blob's digest, and it is the ONLY identity (invariant 1).
	// Not a path, not a release name, not a candidate id.
	Target string
	// Sources is everywhere the bytes might come from, in any order. Plan
	// decides the order actually attempted.
	Sources []Source
	// Urgency is how hard to try. Empty means UrgencyBackground.
	Urgency Urgency
}

// ErrNoSource is a session with nowhere to fetch from.
//
// It is a distinct condition rather than an empty plan so that a caller cannot
// treat "nothing to try" as "tried everything and none worked" — the two lead
// to different actions, and the second is what a silent empty slice looks like.
var ErrNoSource = fmt.Errorf("transfer: this session has no usable source")

// Plan is the order to attempt sources in.
//
// # The order
//
// Health first, then kind, then id — and the whole thing is total and
// deterministic, so two runs of the same fabric attempt the same source first.
// A transfer test can then assert against a named source rather than against
// whichever one a map walk produced (ADR-0017).
//
//	reachable  before  unknown  before  unreachable
//	peer       before  web seed  before  external
//
// Health outranks kind because a reachable web seed beats a peer that has been
// off since Tuesday, whatever their kinds. Within a health class, a peer is
// preferred to a web seed against the SAME machine because the peer path can
// resume by verified chunk (ADR-0035) and a web seed is a plain ranged read;
// both beat external, which costs the outside world's bandwidth and is the
// thing §23 exists to stop doing twice.
//
// # An unreachable source is skipped, never fatal
//
// ADR-0041: a peer that cannot be reached is having an ordinary day, and a
// session makes progress with whoever it has. Nothing here refuses a session
// because a participant is missing, and nothing waits for one.
//
// Under UrgencyInteractive, sources last seen unreachable are left out
// entirely — see Urgency. Under the default they are ordered last and still
// attempted.
func (s Session) Plan() ([]Source, error) {
	out := make([]Source, 0, len(s.Sources))
	for _, src := range s.Sources {
		if src.ID == "" || src.Kind == "" {
			// A source that cannot be named cannot be ordered deterministically
			// and cannot be reported afterwards. Dropped rather than attempted,
			// so "no source" and "a source we could not describe" do not
			// present as the same thing.
			continue
		}
		if s.urgency() == UrgencyInteractive && src.Health == HealthUnreachable {
			continue
		}
		out = append(out, src)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSource, s.Target)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if hi, hj := healthRank(out[i].Health), healthRank(out[j].Health); hi != hj {
			return hi < hj
		}
		if ki, kj := kindRank(out[i].Kind), kindRank(out[j].Kind); ki != kj {
			return ki < kj
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s Session) urgency() Urgency {
	if s.Urgency == "" {
		return UrgencyBackground
	}
	return s.Urgency
}

// healthRank orders the health states. Unknown sits BETWEEN reachable and
// unreachable deliberately: it is the absence of evidence, not evidence of
// absence, and it is what every peer row reads as until something probes it.
func healthRank(state string) int {
	switch state {
	case HealthReachable:
		return 0
	case HealthUnreachable:
		return 2
	default:
		return 1
	}
}

// kindRank prefers the fabric to the outside world. See Plan.
func kindRank(k Kind) int {
	switch k {
	case KindPeer:
		return 0
	case KindWebSeed:
		return 1
	case KindExternal:
		return 2
	default:
		return 3
	}
}
