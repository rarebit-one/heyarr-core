package acquisition

// Route is how a want's bytes are obtained (ADR-0082).
//
// It lives here, in the acquisition leaf, rather than in the strategy package
// that decides it per content type, so that ScheduleFor can take a Route
// without importing strategy — strategy imports acquisition, not the other way
// around. It is the acquisition axis the schedule policy cares about: whether a
// want is something an indexer is ever asked about.
type Route string

const (
	// RouteSearch is the route where the bytes are found by asking indexers and
	// then downloaded. A want on this route is on a search schedule (see
	// ScheduleFor). Movies, series, video channels.
	RouteSearch Route = "search"
	// RouteDirect is the route where the feed already names where the bytes are —
	// a web capture, a podcast enclosure — and they are taken from there. A want
	// on this route is NEVER enqueued for an indexer search: the source of record
	// is the feed's own reference, and an indexer fallback would make the want
	// mean a different set of bytes than the one it was projected for. Documents
	// and podcasts.
	RouteDirect Route = "direct"
	// RouteStore is the route where the bytes come from a store the person
	// already owns the content on — a GOG offline installer, a Steam depot —
	// fetched by that store's own tooling against their library (ADR-0118). A
	// want on this route is NEVER enqueued for an indexer search: what an
	// indexer has for a game is mostly repacks, which are not the owned copy
	// the want asks for. Games.
	RouteStore Route = "store"
)

// Searchable reports whether a want on this route is ever asked of an indexer.
// It is the whole of what ScheduleFor needs to know about a route, named so the
// short-circuit there reads as the fact it is rather than a string compare.
//
// It names the routes that are NOT searched rather than the one that is, so an
// unknown or empty route keeps the conservative reading strategy.For gives an
// unknown content type: ordinary searchable content.
func (r Route) Searchable() bool { return r != RouteDirect && r != RouteStore }
