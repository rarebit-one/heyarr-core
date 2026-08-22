package catalog_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

// The database half of the peer identity (M4-03, ADR-0012).

func TestRecordingThePublicKeyIsIdempotentAndEmitsOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cat.RecordSelfPublicKey(ctx, "ed25519", pub); err != nil {
		t.Fatal(err)
	}
	// Re-recording the SAME key is what a restart does after an interrupted
	// first start. It must converge rather than fail.
	if err := h.cat.RecordSelfPublicKey(ctx, "ed25519", pub); err != nil {
		t.Fatalf("re-recording the same key failed: %v", err)
	}

	id, got, err := h.cat.SelfIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("no self peer")
	}
	if !bytes.Equal(got, pub) {
		t.Errorf("the stored public key is not the one recorded")
	}

	// Invariant 7: the transition emitted an event — once, not twice.
	var n int
	if err := h.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE type = 'peer.identity_established'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("peer.identity_established emitted %d times, want once", n)
	}

	// And the algorithm travelled with the key, so a future rotation is a
	// second accepted value rather than a migration.
	var algo string
	if err := h.db.Reader().QueryRowContext(ctx,
		`SELECT key_algo FROM peers WHERE id = ?`, id).Scan(&algo); err != nil {
		t.Fatal(err)
	}
	if algo != "ed25519" {
		t.Errorf("key_algo = %q, want ed25519", algo)
	}
}

// Overwriting is refused rather than silently accepted: the public key in the
// database is the evidence of which of two contested machines is real, and a
// write that replaces it destroys the only thing that could settle the
// argument (ADR-0010).
func TestRecordingADifferentPublicKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cat.RecordSelfPublicKey(ctx, "ed25519", first); err != nil {
		t.Fatal(err)
	}
	err = h.cat.RecordSelfPublicKey(ctx, "ed25519", second)
	if err == nil {
		t.Fatal("a second, different public key overwrote the first")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error = %v, want it to say the key was not overwritten", err)
	}

	_, stored, err := h.cat.SelfIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, first) {
		t.Error("the original public key did not survive the refused overwrite")
	}
}

// The private key must never have a route into the database. This asserts the
// column set rather than the code: a future migration that adds a private_key
// column fails here, which is the point.
func TestNoPeerColumnHoldsPrivateKeyMaterial(t *testing.T) {
	h := newHarness(t)
	rows, err := h.db.Reader().QueryContext(context.Background(), `PRAGMA table_info(peers)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(columns) == 0 {
		t.Fatal("the peers table has no columns, so this test proves nothing")
	}
	for _, name := range columns {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "private") || strings.Contains(lower, "secret") || lower == "seed" {
			t.Errorf("peers.%s looks like private key material — the private key is a 0600 file, "+
				"never a column, because backups stream to peers (ADR-0003, ADR-0012)", name)
		}
	}
	var found bool
	for _, name := range columns {
		if name == "public_key" {
			found = true
		}
	}
	if !found {
		t.Error("peers.public_key is gone")
	}
}
