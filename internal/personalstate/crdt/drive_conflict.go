package crdt

// drive_conflict.go turns the drive's two-way (and multi-way) conflict DETECTION
// into conflicted-copy RELOCATION (ADR-0095, task W2/#538): when a path has more
// than one live head, the winner stays at the path and every loser becomes a
// visible file at a derived path — "name (conflicted copy — <writer> — <at>).ext"
// — so the merge discards no bytes and the user sees both edits as ordinary files.
//
// # Why this converges byte-for-byte
//
// [Drive.Resolved] is a PURE FUNCTION of the converged entry map: it reads the
// heads (already order-independent — see drive.go) and derives every path from
// fields every replica holds. It emits NO new changes and mutates nothing, so it
// cannot feed back into the CRDT or depend on arrival order. Two replicas that have
// merged the same writes hold the same entry map, so Resolved returns the identical
// tree — the loser at the identical derived path — no matter what order either
// replica applied the writes in.
//
//   - The winner is the greatest (At, Writer) live head, the same total order the
//     rest of the drive uses.
//   - A loser's derived path is a pure function of (its original path, its own
//     total-order key). Distinct losers have distinct keys — two writes with the
//     same (At, Writer) are the SAME entry in the write set — so the derivation is
//     injective across losers and two losers can never land on one path.
//   - The only remaining clash is a derived path equalling a real live path (a
//     user really did name a file that, or a craft attempt). It is broken by a
//     deterministic numeric suffix computed against the FIXED set of occupied live
//     paths, over losers visited in a globally-sorted order, so the disambiguation
//     is itself a pure function of the entry map.

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Resolved returns the conflict-RESOLVED live tree: every live path's winner at
// its own path, plus each losing head of a conflicted path relocated to a derived
// "conflicted copy" path, all as ordinary [DriveEntry] values (Conflicted is
// always false here — relocation has already turned the conflict into two files).
// The result is sorted by path and is a pure, order-independent function of the
// converged drive, so two converged replicas produce byte-identical trees.
//
// It is the read a client renders. [Drive.List]/[Drive.Get] are left as the raw
// view (winner at the path with the Conflicted flag still set) for callers that
// want to detect a conflict rather than see it already relocated.
func (d *Drive) Resolved() []DriveEntry {
	// occupied is every path that carries a live head — the winner sits there, so a
	// derived path must avoid it. Fixed before any relocation, so the collision
	// guard is a pure function of the entry map, not of relocation order.
	occupied := make(map[string]bool, len(d.entries))
	for p, rec := range d.entries {
		if len(rec.liveHeads()) > 0 {
			occupied[p] = true
		}
	}

	out := make([]DriveEntry, 0, len(d.entries))
	type reloc struct {
		orig string
		v    driveValue
	}
	var losers []reloc

	for p, rec := range d.entries {
		live := rec.liveHeads() // ascending by key
		if len(live) == 0 {
			continue
		}
		winner := live[len(live)-1]
		out = append(out, DriveEntry{Path: p, Blob: winner.Blob, Size: winner.Size, MTime: winner.MTime})
		for _, l := range live[:len(live)-1] {
			losers = append(losers, reloc{orig: p, v: l})
		}
	}

	// Visit losers in a globally-sorted order so the numeric collision suffix is
	// deterministic regardless of map iteration (the map's own order is random).
	sort.Slice(losers, func(i, j int) bool {
		if losers[i].orig != losers[j].orig {
			return losers[i].orig < losers[j].orig
		}
		return losers[j].v.key.greater(losers[i].v.key)
	})

	placed := make(map[string]bool, len(losers))
	for _, l := range losers {
		dp := uniqueConflictPath(l.orig, l.v.key, occupied, placed)
		placed[dp] = true
		out = append(out, DriveEntry{Path: dp, Blob: l.v.Blob, Size: l.v.Size, MTime: l.v.MTime})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// uniqueConflictPath derives the relocation path for one losing head and, if it
// would collide with a real live path or an already-placed conflicted copy, breaks
// the tie with a deterministic numeric suffix. Because distinct losers have
// distinct keys the base derivation is already injective across losers, so the loop
// only ever fires against a real live file — a rare, craftable case — and even then
// resolves identically on every replica.
func uniqueConflictPath(orig string, k posKey, occupied, placed map[string]bool) string {
	for n := 1; ; n++ {
		cand := conflictPath(orig, k, n)
		if !occupied[cand] && !placed[cand] {
			return cand
		}
	}
}

// conflictPath builds "dir/stem (conflicted copy — <writer> — <at>).ext" (with a
// " (n)" disambiguator inside the stem for n > 1), splitting the extension off the
// final path element. A leading-dot name with no other dot (".bashrc") is treated
// as having no extension, so its conflicted copy keeps the dotfile spelling.
func conflictPath(orig string, k posKey, n int) string {
	dir, file := path.Split(orig)
	stem, ext := splitExt(file)
	suffix := ""
	if n > 1 {
		suffix = fmt.Sprintf(" (%d)", n)
	}
	name := fmt.Sprintf("%s (conflicted copy — %s — %d)%s%s", stem, k.Writer, k.At, suffix, ext)
	return dir + name
}

// splitExt splits the final path element into its stem and extension. Unlike
// path.Ext it does not treat a leading dot (a dotfile) as an extension boundary.
func splitExt(file string) (stem, ext string) {
	idx := strings.LastIndexByte(file, '.')
	if idx <= 0 { // no dot, or a leading-dot dotfile -> no extension
		return file, ""
	}
	return file[:idx], file[idx:]
}
