package httpapi

// DeviceFailureReason is the closed log/metric label for a rejected device
// credential — deviceFailureReason, exported for the self-enrolment route
// (ADR-0067), which judges a cert and proof by the same verifier and must log
// a refusal under the same bounded label set rather than a second one that
// drifts.
func DeviceFailureReason(err error) string { return deviceFailureReason(err) }
