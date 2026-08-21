package providers

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// The health job's identity (ADR-0025, invariant 4, ADR-0002).
//
// Reachability is checked continuously and it is a JOB, not a goroutine inside
// whatever happens to hold the registry. Roles communicate only through the job
// table and HTTP, even inside `heyarr all` — and a health answer that existed
// only in one process's memory would be invisible to the controller answering
// GET /api/v1/providers when the worker is on another machine.
const (
	// HealthJobType checks every configured provider.
	HealthJobType = "provider_health"
	// HealthDedupeKey is the queue's idempotency key.
	//
	// ONE key for the whole pass rather than one per provider: two concurrent
	// passes would each write what they found while the other was still
	// looking, and the loser would record answers that were already stale. A
	// pass already queued or running is the same pass.
	HealthDedupeKey = "provider-health"
)

// Constructor builds a client for a kind this package cannot import.
//
// # Why this indirection exists
//
// internal/downloads imports THIS package, for the Provider and Downloader
// contracts. So this package cannot import it back to construct a Transmission
// client, and the cycle is not an accident of layering — it is the interface
// boundary working. A registry that reached into every integration would be a
// registry every integration had to be linked into, including in a test that
// wanted none of them.
//
// So the wiring is injected by whoever owns both: the worker and the
// controller. Returning `handled=false` means "not mine", so several
// constructors compose and an unrecognised kind still falls through to the
// honest not-implemented report.
type Constructor func(r Resolved, now func() time.Time) (p Provider, handled bool, err error)

// Build turns validated configuration into a registry.
//
// See BuildWith for the constructor hook; this is the no-integrations form,
// used where only the registry's own behaviour matters.
func Build(resolved []Resolved, log *slog.Logger, now func() time.Time) (*Registry, error) {
	return BuildWith(resolved, log, now, nil)
}

// BuildWith turns validated configuration into a registry, using ctor for the
// kinds this package cannot construct itself.
//
// # Only the kinds that exist
//
// A configured provider whose client is not wired here is not an error: the
// entry is validated, recorded, reported on GET /api/v1/providers, and marked
// unhealthy with a detail saying the client is not built yet.
//
// That is deliberate and it is the honest behaviour. Refusing to start would
// punish an operator for configuring something the roadmap says is coming;
// silently ignoring it would leave them wondering why their indexer is never
// consulted. Saying "configured, not yet implemented" on the status endpoint is
// the answer that costs them nothing and tells them everything.
//
// # A disabled provider is registered and not routed
//
// It still appears in the registry so it still appears in the status endpoint.
// "Why is nothing searching" must be answerable from one request rather than by
// re-reading the config file, and a provider that vanished when disabled would
// make disabled and absent indistinguishable.
func BuildWith(
	resolved []Resolved, log *slog.Logger, now func() time.Time, ctor Constructor,
) (*Registry, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "providers")

	reg := New(now)
	for _, r := range resolved {
		if !r.Enabled {
			// Registered so it is reported, with no capabilities so it is never
			// routed to. A disabled provider is a fact about configuration, and
			// the status endpoint is where facts about configuration go.
			if err := reg.RegisterInert(newDisabled(r)); err != nil {
				return nil, err
			}
			log.Info("a provider is configured but disabled", "provider", r.Name, "type", r.Kind)
			continue
		}

		p, err := construct(r, now, ctor)
		if err != nil {
			return nil, err
		}
		if err := reg.Register(p); err != nil {
			return nil, err
		}
		// The endpoint is logged and the credential is not — and it is not
		// because Secret's LogValue redacts it, not because this line remembered
		// to leave it out. That is the whole point of the type.
		log.Info("registered a provider",
			"provider", r.Name, "type", r.Kind,
			"capabilities", JoinCapabilities(r.Capabilities),
			"endpoint", endpointFor(r))
	}
	return reg, nil
}

func endpointFor(r Resolved) string {
	if r.Endpoint == nil {
		return ""
	}
	return r.Endpoint.String()
}

func construct(r Resolved, now func() time.Time, ctor Constructor) (Provider, error) {
	// An injected constructor gets first refusal. It is what turns a
	// "configured, not implemented" entry into a working client once the
	// integration exists, without this package learning about the integration.
	if ctor != nil {
		p, handled, err := ctor(r, now)
		if err != nil {
			return nil, err
		}
		if handled {
			return p, nil
		}
	}
	switch r.Kind {
	case KindFake:
		f := NewFake(r.Name, r.Capabilities...)
		if now != nil {
			f.now = now
		}
		return f, nil
	case KindTorznab, KindTransmission:
		// No constructor claimed it. The Torznab client lands in M3-09;
		// Transmission's exists but is only wired where something owns both
		// packages. Either way this is a real registry entry with real
		// capabilities that reports honestly rather than pretending.
		return newUnimplemented(r, now), nil
	default:
		// Unreachable: ParseKind refuses anything else. Kept because a kind
		// added without a branch here would otherwise be silently dropped.
		return nil, fmt.Errorf("providers: %q has no constructor for type %q", r.Name, r.Kind)
	}
}

// unimplemented is a configured provider whose client has not been written yet.
//
// It exists so the registry's shape is complete before its members are: routing,
// health and reporting all work, and M3-09 replaces one branch of construct
// rather than teaching the registry a new concept.
type unimplemented struct {
	name string
	kind Kind
	caps []Capability
	now  func() time.Time
}

func newUnimplemented(r Resolved, now func() time.Time) *unimplemented {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &unimplemented{name: r.Name, kind: r.Kind, caps: r.Capabilities, now: now}
}

func (u *unimplemented) Name() string { return u.name }

func (u *unimplemented) Capabilities() []Capability {
	return append([]Capability(nil), u.caps...)
}

// Check reports the truth: configured, and not yet able to do anything.
//
// Unhealthy rather than healthy, because healthy would advertise something it
// cannot deliver — work would route to it and then fail, which ADR-0025 says is
// worse than advertising nothing.
func (u *unimplemented) Check(_ context.Context) Health {
	return Unhealthy(
		fmt.Sprintf("the %s client is not implemented yet", u.kind), u.now())
}

// disabled is a provider an operator has switched off.
//
// It declares no capabilities, so Route never returns it and JobCapabilities
// never counts it — which is what "disabled" has to mean for the degrade path
// to be honest. It is still registered, so it is still reported.
type disabled struct {
	name string
	kind Kind
	// declared is what it WOULD do if enabled, reported so an operator can see
	// what they turned off rather than only that something is off.
	declared []Capability
	now      func() time.Time
}

func newDisabled(r Resolved) *disabled {
	return &disabled{
		name:     r.Name,
		kind:     r.Kind,
		declared: r.Capabilities,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (d *disabled) Name() string { return d.name }

// Capabilities is deliberately EMPTY, not d.declared.
//
// Registry.Register refuses a provider with no capabilities, which is why
// disabled sidesteps it — see Build. Everything else in the registry treats
// "no capabilities" as "never routed to", which is exactly right.
func (d *disabled) Capabilities() []Capability { return nil }

// Declared is what this provider would do if it were enabled.
func (d *disabled) Declared() []Capability { return append([]Capability(nil), d.declared...) }

func (d *disabled) Check(_ context.Context) Health {
	return Unhealthy("disabled in configuration", d.now())
}
