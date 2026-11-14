package backupsync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

func deadlineClient(t *testing.T, budget time.Duration) *http.Client {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: "peer-a"})
	if err != nil {
		t.Fatal(err)
	}
	client, closeIdle, err := NewPusher(material, nil).clientFor(mtls.Peer{PeerID: "peer-b", PublicKey: pub}, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeIdle)
	transport := client.Transport.(*http.Transport)
	if err := mtls.AssertPinned(transport.TLSClientConfig); err != nil {
		t.Fatal(err)
	}
	if transport.DialContext == nil || transport.TLSHandshakeTimeout != mtls.DialTimeout {
		t.Fatal("long transfer budget lost the bounded dial or TLS handshake")
	}
	return client
}

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// No wall-clock sleeps: virtual time advances only when all goroutines block.
// The transport deliberately streams after the old whole-request budget, then
// keeps reading until either the caller or the bounded transfer budget stops it.
func TestBackupClientDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		cancel bool
	}{
		{"transfer", transferTimeout, false},
		{"metadata", mtls.DialTimeout, false},
		{"caller cancellation", transferTimeout, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := deadlineClient(t, tc.budget)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				client.Transport = deadlineTransport(func(req *http.Request) (*http.Response, error) {
					reader, writer := io.Pipe()
					go func() {
						select {
						case <-time.After(mtls.DialTimeout + time.Second):
							_, _ = io.Copy(writer, strings.NewReader("snapshot chunk"))
						case <-req.Context().Done():
						}
						<-req.Context().Done()
						_ = writer.CloseWithError(req.Context().Err())
					}()
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader}, nil
				})
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://peer.invalid/backup", nil)
				if err != nil {
					t.Fatal(err)
				}
				started := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				if tc.cancel {
					cancel()
				}
				data, err := io.ReadAll(resp.Body)
				if tc.cancel {
					if !errors.Is(err, context.Canceled) || time.Since(started) != 0 {
						t.Fatalf("cancel: elapsed=%s err=%v", time.Since(started), err)
					}
				} else {
					if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != tc.budget {
						t.Fatalf("deadline: elapsed=%s err=%v", time.Since(started), err)
					}
				}
				want := ""
				if tc.budget == transferTimeout && !tc.cancel {
					want = "snapshot chunk"
				}
				if string(data) != want {
					t.Fatalf("streamed %q, want %q", data, want)
				}
			})
		})
	}
}

// A slow request body (upload) must be allowed to finish before response headers
// arrive, not just a slow response body (recovery download).
func TestBackupClientAllowsSlowUploadAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := deadlineClient(t, transferTimeout)
		client.Transport = deadlineTransport(func(req *http.Request) (*http.Response, error) {
			select {
			case <-time.After(mtls.DialTimeout + time.Second):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			data, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			if string(data) != "snapshot" {
				t.Errorf("upload body = %q", data)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"generation":1}`))}, nil
		})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://peer.invalid/backup", strings.NewReader("snapshot"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("upload exceeded the metadata budget: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil || string(data) != `{"generation":1}` {
			t.Fatalf("acknowledgement = %q, %v", data, err)
		}
	})
}
