// Package device is this machine's own key store: the client half of §40's
// device identity, and nothing else.
//
// # Why it is here and not in the controller
//
// §40 gives every user device its own private key, and §41 wraps space keys
// for those keys. ADR-0022's enrolment story starts with "an existing
// authorised device" — and Heyarr has no first-party mobile app, so there is no
// such device to start from. The desktop CLI is that device.
//
// Everything in this package therefore runs on the USER's machine, against the
// user's CONFIG directory, and talks to no controller. The server's data
// directory belongs to the service account that runs Heyarr; it is backed up
// with the catalog, restored with the catalog, and read by whoever operates the
// host. A device key put there would be a user's private key living inside the
// server's blast radius, which is exactly what §38 and SECURITY.md exist to
// prevent.
//
// # Why the key lands before it authorises anything
//
// The ADR-0010 argument, applied a second time: a store that exists early, with
// one device and no delegations, means Milestone 8 populates a shape rather
// than retrofitting one. See ADR-0032.
//
// The cost of landing early is that the key is, today, decorative. ADR-0011's
// bearer tokens are still the only thing that authorises a caller against the
// controller. So every record this package produces carries [Device.Unproven]
// and [Device.EnrolmentStatus], and the CLI prints [NotYetAuthorising] — for
// the same reason placement made `unproven` a required RESPONSE field rather
// than a domain note: a caveat that lives only in the domain is one the edge
// forgets. A key called self-sovereign that is not yet load-bearing is worse
// than no key at all, because someone will trust it.
//
// # What is deliberately absent
//
// Wrapped space keys (§41), delegation of a user identity to several device
// keys, capability grants, pairing by short authentication string, and the
// recovery secret are all Milestone 8 and 9. None of them is stubbed here: a
// stub is a published promise with a hole in it (ADR-0019's reasoning).
package device
