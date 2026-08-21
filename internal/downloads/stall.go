package downloads

import (
	"fmt"
	"strings"
)

// Detecting a stalled transfer.
//
// # 🔴 The measured finding this file exists for
//
// A transfer whose only tracker cannot be reached reports, at the TOP level:
//
//	error       = 0
//	errorString = ""
//
// while trackerStats[].lastAnnounceResult says "Could not connect to tracker".
//
// That was measured against a real Transmission 4.1.3, not assumed, and it is
// preserved in the captured corpus as the second torrent in torrent-get —
// alongside a healthy one, so the two are distinguishable rather than merely
// present.
//
// The consequence is severe and quiet. A client that watches errorString — the
// obvious field, and the one its NAME PROMISES — sees a transfer sitting at 0%
// with no error, forever. Nothing goes red. The acquisition stays in
// DOWNLOADING (§64) until somebody looks at the download client by hand and
// wonders why it has been there since Tuesday.
//
// A hand-written fixture would have put a message in errorString, because that
// is what a reasonable person assumes the field is for. This is ADR-0026's
// argument arriving as a concrete fact: the corpus is not a convenience, it is
// the only thing that would ever have told us.
//
// So: errorString is read AND trackerStats is read, and a failure in either is
// a failure. Reading only the first is the bug; reading only the second would
// miss the disk-full and permission errors that DO surface at the top level.

// announceSuccess is what Transmission reports for a tracker that answered.
//
// Compared as a literal because it is a literal: Transmission writes the
// English string "Success" into lastAnnounceResult, and there is no numeric
// code beside it that means the same thing. Matching on it is unpleasant and
// it is what the protocol offers.
const announceSuccess = "Success"

// Trouble describes why a transfer is not progressing.
//
// A value rather than an error because it is an OBSERVATION the poll job
// records against an acquisition, not a failure of the call that found it.
type Trouble struct {
	// Reason is machine-readable, so the poll job can branch and a future
	// operator-facing view can group. Prose changes; these do not.
	Reason TroubleReason
	// Detail is the human half, naming what was observed.
	Detail string
}

// TroubleReason is the stable code.
type TroubleReason string

const (
	// TroubleClientError is a failure Transmission reports at the top level —
	// a disk full, a permission problem, a corrupt resume file.
	TroubleClientError TroubleReason = "client_error"
	// TroubleTrackerUnreachable is the invisible one: every tracker failed to
	// answer while the transfer reports no error of its own.
	TroubleTrackerUnreachable TroubleReason = "tracker_unreachable"
)

// inspect reports what is wrong with a transfer, if anything.
//
// Order matters. A top-level error is checked FIRST because when both are
// present it is the more specific answer: a transfer that cannot write to disk
// will also stop announcing, and reporting the tracker would send an operator
// to the network rather than to the disk.
func inspect(t torrent) (Trouble, bool) {
	if strings.TrimSpace(t.ErrorString) != "" {
		return Trouble{
			Reason: TroubleClientError,
			Detail: t.ErrorString,
		}, true
	}

	// The invisible case. Only when there is at least one tracker AND every
	// one of them has actually tried and failed.
	//
	// "Has actually tried" is load-bearing: a transfer added five seconds ago
	// has trackers that have not announced yet, and calling that unreachable
	// would report every new acquisition as troubled for its first few
	// seconds. hasAnnounced is what separates "failed" from "not yet".
	var announced, failed int
	for _, ts := range t.TrackerStats {
		if !ts.HasAnnounced {
			continue
		}
		announced++
		if !ts.LastAnnounceSucceeded ||
			(ts.LastAnnounceResult != "" && ts.LastAnnounceResult != announceSuccess) {
			failed++
		}
	}
	if announced > 0 && failed == announced {
		return Trouble{
			Reason: TroubleTrackerUnreachable,
			Detail: fmt.Sprintf("every tracker failed to answer (%s)", firstFailure(t)),
		}, true
	}
	return Trouble{}, false
}

// firstFailure names one tracker's complaint, for the detail.
//
// One rather than all: a transfer with eight dead trackers produces eight
// copies of the same sentence, and the first is the one an operator will act
// on.
func firstFailure(t torrent) string {
	for _, ts := range t.TrackerStats {
		if ts.HasAnnounced && ts.LastAnnounceResult != "" &&
			ts.LastAnnounceResult != announceSuccess {
			return ts.LastAnnounceResult
		}
	}
	return "no tracker answered"
}
