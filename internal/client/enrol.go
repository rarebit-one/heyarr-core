package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// EnrolPath is where a paired device enrols itself at a peer (ADR-0067). It is
// OUTSIDE APIPrefix, on the unauthenticated router, and repeated here rather
// than imported for the reason APIPrefix is.
const EnrolPath = "/enrol"

// EnrolRequest is the POST /enrol body: the device's admitting op, a fresh
// possession proof over it, a display name, and the membership ops the device
// knows (ADR-0068), so a device admitted by a member the peer has never met can
// still be judged.
type EnrolRequest struct {
	Op    string   `json:"op"`
	Proof string   `json:"proof"`
	Name  string   `json:"name"`
	Ops   []string `json:"ops,omitempty"`
}

// EnrolledDevice is the POST /enrol response: the device as the peer now knows
// it, including the identity's recovery encryption PUBLIC key when it has one.
type EnrolledDevice struct {
	DeviceKey             string    `json:"device_key"`
	EncryptionKey         string    `json:"encryption_key,omitempty"`
	RecoveryEncryptionKey string    `json:"recovery_encryption_key,omitempty"`
	Name                  string    `json:"name"`
	User                  string    `json:"user"`
	AdmittedBy            string    `json:"admitted_by"`
	EnrolledAt            time.Time `json:"enrolled_at"`
	ExpiresAt             time.Time `json:"expires_at"`
}

// Enrol posts a self-enrolment to the peer. The route takes no Authorization
// header: the request body IS the credential, so the client's bearer token, if
// it has one, is not sent. A refusal is the peer's opaque 401 (ADR-0067); the
// reason is in the peer's log, not the response.
func (c *Client) Enrol(ctx context.Context, body EnrolRequest) (EnrolledDevice, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return EnrolledDevice{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+EnrolPath, bytes.NewReader(buf))
	if err != nil {
		return EnrolledDevice{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.agent)
	var out EnrolledDevice
	if err := c.roundTrip(req, &out); err != nil {
		return EnrolledDevice{}, err
	}
	return out, nil
}
