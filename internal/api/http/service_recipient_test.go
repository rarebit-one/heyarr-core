// Service recipients (ADR-0104), proven through the real middleware chain and
// the real personal-state API: only a device registers an executor's key; a
// grant carries the wrap for it in one transaction; the executor is shown that
// wrap and no other; a rotation must account for it; and a revoke deletes it.
//
//nolint:bodyclose // responses are closed in rh.do
package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/servicerecipient"
)

func TestServiceRecipientLifecycle(t *testing.T) {
	t.Parallel()
	h := newRestrictedHarness(t)
	sp := h.spaceID
	execKey := "x25519:" + strings.Repeat("44", 32)
	strayKey := "x25519:" + strings.Repeat("55", 32)
	fp, err := servicerecipient.Fingerprint(execKey)
	if err != nil {
		t.Fatal(err)
	}
	register := map[string]any{"principal": h.executor2Name, "recipient": execKey, "label": "home executor", "fingerprint": fp}

	// Registration is a device's consent: never a bearer, never the executor.
	h.mustStatus(t, "POST", "/service-recipients", h.service, register, http.StatusForbidden)
	h.mustStatus(t, "POST", "/service-recipients", h.ungranted, register, http.StatusForbidden)
	bad := map[string]any{"principal": h.executor2Name, "recipient": execKey, "fingerprint": "AAAA AAAA AAAA AAAA"}
	if body := h.mustStatus(t, "POST", "/service-recipients", h.deviceCredential(), bad, http.StatusBadRequest); !strings.Contains(body, psapi.CodeFingerprintMismatch) {
		t.Errorf("a wrong fingerprint was refused without its code: %s", body)
	}
	h.mustStatus(t, "POST", "/service-recipients", h.deviceCredential(),
		map[string]any{"principal": h.executor2Name, "recipient": "x25519:zz"}, http.StatusBadRequest)
	h.mustStatus(t, "POST", "/service-recipients", h.deviceCredential(),
		map[string]any{"principal": h.executor2Name, "recipient": ownerRecipient}, http.StatusConflict)
	h.mustStatus(t, "POST", "/service-recipients", h.deviceCredential(),
		map[string]any{"principal": "player", "recipient": execKey}, http.StatusBadRequest)
	var sr struct {
		ID, Recipient, Fingerprint string
	}
	if err := json.Unmarshal([]byte(h.mustStatus(t, "POST", "/service-recipients", h.deviceCredential(), register, http.StatusCreated)), &sr); err != nil {
		t.Fatal(err)
	}
	if sr.Fingerprint != fp || sr.Recipient != execKey {
		t.Errorf("registered %+v, want the key and its fingerprint", sr)
	}
	h.mustStatus(t, "POST", "/service-recipients", h.deviceCredential(), register, http.StatusOK)
	h.mustStatus(t, "GET", "/service-recipients", h.ungranted, nil, http.StatusForbidden)
	if body := h.mustStatus(t, "GET", "/service-recipients", h.service, nil, http.StatusOK); !strings.Contains(body, execKey) {
		t.Errorf("the listing lacks the registered key: %s", body)
	}

	// A registered but ungranted key is no wrap target, and a grant carrying a
	// wrap for a key that is not the executor's records nothing at all.
	rewrap := func(key string) map[string]any {
		return map[string]any{"wrapped_keys": []map[string]any{{"recipient": key, "wrapped": []byte("copy")}}}
	}
	if body := h.mustStatus(t, "POST", "/spaces/"+sp+"/keys", h.deviceCredential(), rewrap(execKey), http.StatusForbidden); !strings.Contains(body, psapi.CodeWrapRecipientNotAllowed) {
		t.Errorf("an ungranted service recipient's wrap was refused without the code: %s", body)
	}
	grantWith := func(key string) map[string]any {
		return map[string]any{
			"principal": h.executor2Name, "caps": "read",
			"wrapped_keys": []map[string]any{{"recipient": key, "wrapped": []byte("exec copy"), "epoch": 0}},
		}
	}
	h.mustStatus(t, "POST", "/spaces/"+sp+"/grants", h.deviceCredential(), grantWith(strayKey), http.StatusForbidden)
	h.mustStatus(t, "GET", "/spaces/"+sp+"/keys", h.ungranted, nil, http.StatusNotFound)

	// The consent act: grant and wrap together.
	var g struct {
		WrappedFor []string `json:"wrapped_for"`
	}
	if err := json.Unmarshal([]byte(h.mustStatus(t, "POST", "/spaces/"+sp+"/grants", h.deviceCredential(), grantWith(execKey), http.StatusCreated)), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.WrappedFor) != 1 || g.WrappedFor[0] != execKey {
		t.Errorf("the grant wrapped for %v", g.WrappedFor)
	}
	var keys struct {
		KeyEpoch    int `json:"key_epoch"`
		WrappedKeys []struct {
			Recipient string
			Epoch     int
		} `json:"wrapped_keys"`
	}
	if err := json.Unmarshal([]byte(h.mustStatus(t, "GET", "/spaces/"+sp+"/keys", h.ungranted, nil, http.StatusOK)), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys.WrappedKeys) != 1 || keys.WrappedKeys[0].Recipient != execKey {
		t.Fatalf("the executor saw %+v, want only its own wrap", keys.WrappedKeys)
	}
	// Another executor granted on the same space still sees none of it.
	if err := json.Unmarshal([]byte(h.mustStatus(t, "GET", "/spaces/"+sp+"/keys", h.granted, nil, http.StatusOK)), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys.WrappedKeys) != 0 {
		t.Errorf("executor-a saw another executor's wrap: %+v", keys.WrappedKeys)
	}
	// Now granted, a re-wrap for the key passes enrol-before-wrap; a stray key
	// still does not.
	h.mustStatus(t, "POST", "/spaces/"+sp+"/keys", h.deviceCredential(), rewrap(execKey), http.StatusOK)
	h.mustStatus(t, "POST", "/spaces/"+sp+"/keys", h.deviceCredential(), rewrap(strayKey), http.StatusForbidden)

	// A rotation must account for the executor's copy (#703): forgetting it is a
	// 409, re-wrapping it carries the executor to the next epoch.
	rotate := func(wraps []string, revoke []string) map[string]any {
		ws := []map[string]any{}
		for _, k := range wraps {
			ws = append(ws, map[string]any{"recipient": k, "wrapped": []byte("e1 copy")})
		}
		return map[string]any{"expected_epoch": 0, "sealed_prev": []byte("sealed"), "wrapped_keys": ws, "revoke": revoke}
	}
	if body := h.mustStatus(t, "POST", "/spaces/"+sp+"/rotate", h.service,
		rotate([]string{ownerRecipient, otherRecipient}, []string{}), http.StatusConflict); !strings.Contains(body, psapi.CodeRotationRecipientsChanged) {
		t.Errorf("a rotation forgetting the executor: %s", body)
	}
	h.mustStatus(t, "POST", "/spaces/"+sp+"/rotate", h.service,
		rotate([]string{ownerRecipient, otherRecipient, execKey}, []string{}), http.StatusOK)
	if err := json.Unmarshal([]byte(h.mustStatus(t, "GET", "/spaces/"+sp+"/keys", h.ungranted, nil, http.StatusOK)), &keys); err != nil {
		t.Fatal(err)
	}
	if keys.KeyEpoch != 1 || len(keys.WrappedKeys) != 1 || keys.WrappedKeys[0].Epoch != 1 {
		t.Errorf("after the rotation the executor saw epoch %d, %+v", keys.KeyEpoch, keys.WrappedKeys)
	}

	// Revoking the grant closes both gates: the executor's next fetch is a 404,
	// and its copy is gone for everyone.
	h.mustStatus(t, "DELETE", "/spaces/"+sp+"/grants/"+h.executor2Name, h.deviceCredential(), nil, http.StatusNoContent)
	h.mustStatus(t, "GET", "/spaces/"+sp+"/keys", h.ungranted, nil, http.StatusNotFound)
	if body := h.mustStatus(t, "GET", "/spaces/"+sp+"/keys", h.service, nil, http.StatusOK); strings.Contains(body, execKey) {
		t.Errorf("the revoked executor's copy survived: %s", body)
	}
	h.mustStatus(t, "POST", "/spaces/"+sp+"/keys", h.deviceCredential(), rewrap(execKey), http.StatusForbidden)

	// Removing the registration is a device's act too.
	h.mustStatus(t, "DELETE", "/service-recipients/"+sr.ID, h.service, nil, http.StatusForbidden)
	h.mustStatus(t, "DELETE", "/service-recipients/"+sr.ID, h.deviceCredential(), nil, http.StatusOK)
	h.mustStatus(t, "DELETE", "/service-recipients/"+sr.ID, h.deviceCredential(), nil, http.StatusNotFound)
}
