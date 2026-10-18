package crdt

// kind.go names the CRDT kinds that can share one space (ADR-0113). A change
// carries its kind in a plaintext `k` field inside the ciphertext; a fold skips a
// change of another kind silently — it is neither malformed nor a head this kind's
// writes parent on — so a vault space can hold a drive, the albums over it and its
// owner's device-policy records without a server change (Invariant 6).
//
// An untagged change predates the tag and is read by shape, a pure function of its
// bytes: a change with a path is a drive change, one with an album id an album
// change. Each kind's fold applies that rule to its own decoded struct.

import "encoding/json"

// The `k` values of ADR-0113.
const (
	KindDrive  = "drive"
	KindAlbum  = "album"
	KindPolicy = "policy"
)

// foreignKind reads only the `k` tag of a change's wire bytes and reports whether
// it names a kind other than own. Each kind's UnmarshalJSON calls it FIRST, so a
// foreign payload whose fields clash with this kind's types (a string `op`, say)
// is never decoded into this kind's struct and cannot fail the decode of a
// mixed-kind space. An untagged change is not foreign here; it is read by shape.
func foreignKind(b []byte, own string) (string, bool, error) {
	var probe struct {
		Kind string `json:"k"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return "", false, err
	}
	return probe.Kind, probe.Kind != "" && probe.Kind != own, nil
}
