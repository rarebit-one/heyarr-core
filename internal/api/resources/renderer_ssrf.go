package resources

// SSRF guard for client-reported renderer URLs (ADR-0111 §6).
//
// A client tells us "there is a renderer at this URL, please fetch its
// description". If we fetched any URL the client provided, we would be an
// open relay to any host the attacker can reach from inside the server process
// — including internal services, cloud metadata endpoints and anything on a
// private network the server is part of. The guard ensures we only fetch
// URLs that describe a MediaRenderer inside a configured site.

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/config"
)

// ssrfError is the stable reason code vocabulary for renderer-report rejections.
// A client switches on the Code field of the problem document.
const (
	// ssrfCodeBadScheme is returned when the URL scheme is not "http".
	ssrfCodeBadScheme = "renderer-url-bad-scheme"
	// ssrfCodeHostname is returned when the host part is not a literal IP address.
	ssrfCodeHostname = "renderer-url-hostname-not-allowed"
	// ssrfCodeUserinfo is returned when the URL carries a userinfo component.
	ssrfCodeUserinfo = "renderer-url-userinfo-not-allowed"
	// ssrfCodeZeroPort is returned when the port is explicitly "0".
	ssrfCodeZeroPort = "renderer-url-zero-port"
	// ssrfCodeOutsideSites is returned when the IP is not inside any configured site.
	// The caller may store the entry as r3_only (Phase 2 relay candidate).
	ssrfCodeOutsideSites = "renderer-url-outside-sites"
)

// ssrfResult distinguishes the three outcomes of checkRendererURL.
type ssrfResult int

const (
	ssrfOK        ssrfResult = iota // URL is safe to fetch
	ssrfR3Only                      // IP outside sites — r3_only candidate, not fetchable now
	ssrfRejected                    // Hard rejection (scheme, hostname, etc.)
)

// rendererURLError carries a stable code alongside the human-readable detail.
// It is what checkRendererURL returns; callers translate it to a problem.Problem.
type rendererURLError struct {
	code   string
	detail string
}

func (e *rendererURLError) Error() string { return e.detail }

// checkRendererURL validates a client-supplied renderer description URL.
//
// It is a pure function: it takes a URL and a SiteMap and returns whether the
// URL is safe to fetch (ssrfOK), should be stored as r3_only (ssrfR3Only),
// or must be hard-rejected (ssrfRejected).
//
//   - scheme must be "http"
//   - host must be a literal IP (no hostname resolution)
//   - no userinfo component
//   - explicit port must not be "0" (any other port, or no port = default 80, is fine)
//   - IP must be inside a configured site network (or, in single-site mode,
//     non-public) → ssrfOK; otherwise → ssrfR3Only (deferred relay candidate)
func checkRendererURL(raw string, sites config.SiteMap) (ssrfResult, *rendererURLError) {
	u, err := url.Parse(raw)
	if err != nil {
		return ssrfRejected, &rendererURLError{
			code:   ssrfCodeBadScheme,
			detail: fmt.Sprintf("renderer URL %q is not a valid URL: %v", raw, err),
		}
	}

	// Scheme must be plain http. https would require TLS to a device on the LAN
	// that almost certainly doesn't have a trusted cert, and any other scheme is
	// not an HTTP server at all.
	if u.Scheme != "http" {
		return ssrfRejected, &rendererURLError{
			code:   ssrfCodeBadScheme,
			detail: fmt.Sprintf("renderer URL scheme must be \"http\", got %q", u.Scheme),
		}
	}

	// Userinfo (http://user:pass@host) must not appear. It has no meaning for
	// a UPnP description endpoint and is a sign the caller is trying to
	// smuggle credentials into the fetch.
	if u.User != nil {
		return ssrfRejected, &rendererURLError{
			code:   ssrfCodeUserinfo,
			detail: "renderer URL must not contain a userinfo component",
		}
	}

	// Extract the host. url.Hostname() strips brackets from IPv6.
	host := u.Hostname()
	if host == "" {
		return ssrfRejected, &rendererURLError{
			code:   ssrfCodeHostname,
			detail: "renderer URL has no host",
		}
	}

	// Host must be a LITERAL IP address — we must not resolve any hostname.
	// If the caller provides a hostname, reject it outright: resolving it would
	// hand the attacker DNS-based SSRF.
	ip, parseErr := netip.ParseAddr(host)
	if parseErr != nil {
		return ssrfRejected, &rendererURLError{
			code:   ssrfCodeHostname,
			detail: fmt.Sprintf("renderer URL host must be a literal IP address (no hostnames), got %q", host),
		}
	}
	ip = ip.Unmap() // normalise IPv4-in-IPv6

	// Port, if present, must not be the zero port.
	if port := u.Port(); strings.TrimSpace(port) == "0" {
		return ssrfRejected, &rendererURLError{
			code:   ssrfCodeZeroPort,
			detail: "renderer URL port must not be 0",
		}
	}

	// Site check: the IP must be inside a configured site (or, in single-site
	// mode, be a non-public address). If it is not, store as r3_only so it can
	// be relayed in Phase 2, but do NOT fetch it now.
	site := sites.SiteOf(ip)
	if site == "away" {
		return ssrfR3Only, &rendererURLError{
			code:   ssrfCodeOutsideSites,
			detail: fmt.Sprintf("renderer URL host %s is outside all configured site networks; it may be relayed in a future release", ip),
		}
	}

	return ssrfOK, nil
}
