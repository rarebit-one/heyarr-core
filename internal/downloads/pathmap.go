package downloads

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Path mapping — translating the download client's filesystem namespace into
// Heyarr's.
//
// # The most common operational failure in this class of software
//
// Transmission reports where it put the bytes IN ITS OWN namespace. If it runs
// in a container, or on another host, or under a different mount, that path is
// not the path Heyarr sees. The captured instance says `/downloads/complete`,
// which is a container path and exists nowhere on the host under that name.
//
// It fails nastily: the transfer completes, the client says done, and ingest
// reports a file that is not there. The *arr ecosystem calls this "remote path
// mapping" and it is the single most-asked support question it has.
//
// # A dumb prefix substitution is exactly the right amount of clever
//
// TRaSH's guide describes Sonarr's implementation as "a dumb find Remote Path
// and replaces it with the Local Path", and that is worth stealing verbatim.
// Anything cleverer — inferring mounts, walking to find the file, matching by
// basename — turns a configuration mistake into a heuristic that is right most
// of the time, which is worse than one that is wrong visibly.
//
// # Two refinements over the prior art
//
// It is a LIST, ordered, because a client can be told to put different
// categories in different places — and longest-prefix-first, so a mapping of
// `/downloads` and one of `/downloads/complete` behave the way an operator
// expects rather than the way their file happened to be ordered.
//
// It lives on the PROVIDER, not in a global table keyed by host. Sonarr keys
// its mappings by host string, which breaks the moment a client is reachable by
// two names, and puts them on a different settings page from the client they
// describe — so the two drift. §59 centralises provider configuration
// precisely to stop that: the mapping is part of how you reach this provider.

// Mapping is one prefix substitution.
type Mapping struct {
	// Remote is the prefix as the download client reports it.
	Remote string `koanf:"remote"`
	// Local is what Heyarr should use instead.
	Local string `koanf:"local"`
}

// PathMap is an ordered set of substitutions.
//
// Ordered by REMOTE PREFIX LENGTH, longest first, rather than by the order an
// operator wrote them. Two mappings can both match — `/downloads` and
// `/downloads/complete` — and the more specific one is always what was meant.
// Leaving it to configuration order would make a correct configuration
// depend on a detail nobody thinks about.
type PathMap []Mapping

// ParsePathMap validates and orders a configured set.
//
// Every failure here is a typo somebody can fix in ten seconds, and every one
// of them would otherwise surface as "ingest cannot find the file" hours later.
func ParsePathMap(name string, in []Mapping) (PathMap, error) {
	out := make(PathMap, 0, len(in))
	seen := map[string]bool{}

	for i, m := range in {
		remote := strings.TrimSpace(m.Remote)
		local := strings.TrimSpace(m.Local)
		if remote == "" {
			return nil, fmt.Errorf("provider %q: path_map[%d] has no remote prefix", name, i)
		}
		if local == "" {
			return nil, fmt.Errorf("provider %q: path_map[%d] has no local prefix", name, i)
		}
		if !path.IsAbs(remote) {
			return nil, fmt.Errorf(
				"provider %q: path_map[%d] remote %q must be an absolute path — "+
					"it is a prefix of what the download client reports, and that is absolute",
				name, i, remote)
		}
		if !strings.HasPrefix(local, "/") {
			return nil, fmt.Errorf(
				"provider %q: path_map[%d] local %q must be an absolute path", name, i, local)
		}
		remote = path.Clean(remote)
		local = path.Clean(local)
		if seen[remote] {
			// Two mappings for one prefix means one of them never applies, and
			// which one is an accident of ordering.
			return nil, fmt.Errorf(
				"provider %q: path_map maps %q twice", name, remote)
		}
		seen[remote] = true
		out = append(out, Mapping{Remote: remote, Local: local})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return len(out[i].Remote) > len(out[j].Remote)
	})
	return out, nil
}

// Resolve translates a client-reported path into one Heyarr can open.
//
// Returns false when nothing matched. That is NOT silently the identity: a
// caller has to decide whether an unmapped path means "the client and Heyarr
// share a filesystem, use it as-is" or "this is misconfigured", and those are
// different situations. Making Resolve return the input would collapse them and
// produce the exact silent failure this file exists to prevent.
func (p PathMap) Resolve(remote string) (string, bool) {
	if len(p) == 0 {
		return "", false
	}
	cleaned := path.Clean(remote)
	for _, m := range p {
		if cleaned == m.Remote {
			return m.Local, true
		}
		// The separator check is what stops `/downloads` matching
		// `/downloads-old`, which is a real directory name and would otherwise
		// be rewritten into somewhere it has no business being.
		if strings.HasPrefix(cleaned, m.Remote+"/") {
			return path.Join(m.Local, strings.TrimPrefix(cleaned, m.Remote+"/")), true
		}
	}
	return "", false
}

// Describe renders the mappings for a log line or a health detail.
func (p PathMap) Describe() string {
	if len(p) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(p))
	for _, m := range p {
		parts = append(parts, m.Remote+" -> "+m.Local)
	}
	return strings.Join(parts, ", ")
}
