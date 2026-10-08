// Package vaultref is the wire form of a reference into an owner's encrypted
// vault, as an outside system that never decrypts holds it (ADR-0104):
//
//	hv1:<space_uuid>               a collection: one vault space
//	hv1:<space_uuid>/<object_uuid> one sealed object in it
//
// The grammar is the one the referring system's contract pins,
// ^hv1:[0-9a-f-]{36}(/[0-9a-f-]{36})?$, and Parse accepts no string that
// pattern refuses. It is stricter in one way only: each part must also be a
// canonical lowercase UUID, because a space id is one and New mints objects as
// one. Every ref New returns matches both.
//
// An object lives in the space's drive at ObjectDir/<object_uuid>.json. The
// name is random, so nothing about the object's content reaches the drive
// path, which is itself encrypted state the server never sees.
package vaultref

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
)

// Scheme is the version prefix of every ref.
const Scheme = "hv1:"

// ObjectDir is the drive directory sealed objects live in.
const ObjectDir = ".jumpdrive/objects"

// pattern is the referring contract's grammar, verbatim.
var pattern = regexp.MustCompile(`^hv1:([0-9a-f-]{36})(?:/([0-9a-f-]{36}))?$`)

// ErrMalformed is a string that is not a vault ref.
var ErrMalformed = errors.New("vaultref: a vault ref is hv1:<space uuid> or hv1:<space uuid>/<object uuid>, lowercase")

// Ref is a parsed vault ref. Object is "" for a collection ref.
type Ref struct {
	Space  string
	Object string
}

// Parse validates s and returns its parts.
func Parse(s string) (Ref, error) {
	m := pattern.FindStringSubmatch(s)
	if m == nil {
		return Ref{}, ErrMalformed
	}
	r := Ref{Space: m[1], Object: m[2]}
	if !canonical(r.Space) || (r.Object != "" && !canonical(r.Object)) {
		return Ref{}, ErrMalformed
	}
	return r, nil
}

// ParseSpace accepts a collection ref or a bare space UUID and returns the
// space id. An object ref is refused: it names more than a space.
func ParseSpace(s string) (string, error) {
	if canonical(s) {
		return s, nil
	}
	r, err := Parse(s)
	if err != nil {
		return "", err
	}
	if r.Object != "" {
		return "", fmt.Errorf("vaultref: %q names an object, not a collection", s)
	}
	return r.Space, nil
}

// New mints a ref for a fresh object in space: a random (version 4) UUID, so
// the name carries no time and no content.
func New(space string) (Ref, error) {
	if !canonical(space) {
		return Ref{}, fmt.Errorf("vaultref: %q is not a space id", space)
	}
	obj, err := uuid.NewRandom()
	if err != nil {
		return Ref{}, err
	}
	return Ref{Space: space, Object: obj.String()}, nil
}

// IsObject reports whether r names one object rather than a collection.
func (r Ref) IsObject() bool { return r.Object != "" }

// String renders the wire form.
func (r Ref) String() string {
	if r.Object == "" {
		return Scheme + r.Space
	}
	return Scheme + r.Space + "/" + r.Object
}

// Path is the drive path of the object r names, or "" for a collection ref.
func (r Ref) Path() string {
	if r.Object == "" {
		return ""
	}
	return ObjectDir + "/" + r.Object + ".json"
}

// canonical reports whether s is a UUID in its canonical lowercase form.
func canonical(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}
