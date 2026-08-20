// Package auth issues, stores and verifies the Milestone 1 API credentials:
// opaque bearer tokens, argon2id-hashed at rest, carrying one of three scopes
// (ADR-0011).
//
// This is deliberately not an identity system. There are no users, no
// sessions, no password login and no OIDC — those are Milestone 8 and would be
// thrown away. What exists here is the minimum that makes an HTTP server which
// range-serves an entire media library safe to run: a credential that can be
// revoked, that expires, that cannot be recovered from the database, and that
// says what it is allowed to do.
package auth

import (
	"fmt"
	"sort"
	"strings"
)

// Scope is a coarse permission. Three levels, ordered, and that is the whole
// model: a finer-grained permission system without an identity model behind it
// is elaborate rather than safe.
type Scope string

// The scopes, in increasing order of authority.
const (
	// ScopeRead may read the catalog and read blob bytes.
	ScopeRead Scope = "read"
	// ScopeWrite may additionally change catalog and desired state.
	ScopeWrite Scope = "write"
	// ScopeAdmin may additionally manage peers, tokens and the system itself.
	ScopeAdmin Scope = "admin"
)

// rank orders the scopes. admin implies write implies read, so a token needs
// only its highest scope — but the storage format is a list, because Milestone
// 8 replaces this with real capability grants and a list survives that.
var rank = map[Scope]int{ScopeRead: 1, ScopeWrite: 2, ScopeAdmin: 3}

// ParseScope validates one scope name.
func ParseScope(s string) (Scope, error) {
	sc := Scope(strings.TrimSpace(strings.ToLower(s)))
	if _, ok := rank[sc]; !ok {
		return "", fmt.Errorf("auth: %q is not a scope — use read, write or admin", s)
	}
	return sc, nil
}

// ParseScopes parses a comma-separated scope list, rejecting anything unknown
// rather than ignoring it. Silently dropping an unrecognised scope is how an
// operator ends up believing a token has permissions it does not have.
func ParseScopes(s string) ([]Scope, error) {
	var out []Scope
	seen := map[Scope]bool{}
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		sc, err := ParseScope(part)
		if err != nil {
			return nil, err
		}
		if !seen[sc] {
			seen[sc] = true
			out = append(out, sc)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("auth: no scopes given — a token with no scope can do nothing")
	}
	return Sort(out), nil
}

// Sort orders scopes least-authoritative first, so the stored and printed form
// is stable and two tokens with the same permissions look the same.
func Sort(scopes []Scope) []Scope {
	out := append([]Scope(nil), scopes...)
	sort.Slice(out, func(i, j int) bool { return rank[out[i]] < rank[out[j]] })
	return out
}

// Join renders scopes for storage and display.
func Join(scopes []Scope) string {
	parts := make([]string, 0, len(scopes))
	for _, s := range Sort(scopes) {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ",")
}

// Split parses a stored scope list. Unlike ParseScopes it tolerates unknown
// entries by dropping them: a row written by a newer binary must not make an
// older one fail open *or* fail closed unpredictably — an unknown scope simply
// grants nothing.
func Split(s string) []Scope {
	var out []Scope
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := rank[Scope(part)]; ok {
			out = append(out, Scope(part))
		}
	}
	return Sort(out)
}

// Allows reports whether a token holding held may perform an action requiring
// want. Authority is ordered — admin implies write implies read — so a token
// granted only `admin` can still read.
func Allows(held []Scope, want Scope) bool {
	need := rank[want]
	if need == 0 {
		return false // an unknown requirement is never satisfied
	}
	for _, h := range held {
		if rank[h] >= need {
			return true
		}
	}
	return false
}
