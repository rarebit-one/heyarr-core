package discovery

import (
	"net"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestParamsTXT(t *testing.T) {
	tests := []struct {
		name string
		p    Params
		want []string
	}{
		{
			name: "tls on with an api path",
			p:    Params{Port: 7777, TLS: true, APIPath: "/api/v1"},
			want: []string{"txtvers=1", "path=/api/v1", "tls=1"},
		},
		{
			name: "tls off",
			p:    Params{Port: 8080, TLS: false, APIPath: "/api/v1"},
			want: []string{"txtvers=1", "path=/api/v1", "tls=0"},
		},
		{
			name: "empty path falls back to root",
			p:    Params{Port: 7777, TLS: true},
			want: []string{"txtvers=1", "path=/", "tls=1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.TXT()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("TXT() = %v, want %v", got, tt.want)
			}
			// txtvers must lead so a client reads the format version before
			// trusting any other key (RFC 6763 §6.4).
			if got[0] != "txtvers=1" {
				t.Errorf("txtvers is not first: %v", got)
			}
		})
	}
}

func TestMessageRejectsUnreachableAdvertisements(t *testing.T) {
	addr := []net.IP{net.ParseIP("192.0.2.10")}
	if _, err := (Params{Port: 0, APIPath: "/api/v1"}).Message(addr, 0); err == nil {
		t.Error("advertising port 0 should be refused")
	}
	if _, err := (Params{Port: 7777, APIPath: "/api/v1"}).Message(nil, 0); err == nil {
		t.Error("advertising with no address should be refused")
	}
}

// TestMessageContents builds an announcement from documentation-range addresses
// and reads the packet back, asserting the DNS-SD quartet is correct and that
// nothing host- or person-identifying is on the wire.
func TestMessageContents(t *testing.T) {
	p := Params{Port: 7777, TLS: true, APIPath: "/api/v1"}
	v4 := net.ParseIP("192.0.2.10")  // TEST-NET-1 (RFC 5737)
	v6 := net.ParseIP("2001:db8::1") // documentation prefix (RFC 3849)
	packed, err := p.Message([]net.IP{v4, v6}, 0)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}

	var msg dnsmessage.Message
	if err := msg.Unpack(packed); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if !msg.Response || !msg.Authoritative {
		t.Error("announcement must be an authoritative response")
	}
	if len(msg.Questions) != 0 {
		t.Errorf("an unsolicited announcement carries no questions, got %d", len(msg.Questions))
	}

	var gotPTR, gotSRV, gotTXT bool
	var gotA, gotAAAA bool
	for _, ans := range msg.Answers {
		switch b := ans.Body.(type) {
		case *dnsmessage.PTRResource:
			gotPTR = true
			if got := ans.Header.Name.String(); got != ServiceName() {
				t.Errorf("PTR owner = %q, want %q", got, ServiceName())
			}
			if got := b.PTR.String(); got != InstanceName() {
				t.Errorf("PTR target = %q, want %q", got, InstanceName())
			}
		case *dnsmessage.SRVResource:
			gotSRV = true
			if b.Port != 7777 {
				t.Errorf("SRV port = %d, want 7777", b.Port)
			}
			if got := b.Target.String(); got != hostLabel {
				t.Errorf("SRV target = %q, want %q", got, hostLabel)
			}
		case *dnsmessage.TXTResource:
			gotTXT = true
			if !reflect.DeepEqual(b.TXT, p.TXT()) {
				t.Errorf("TXT = %v, want %v", b.TXT, p.TXT())
			}
		case *dnsmessage.AResource:
			gotA = true
			if got := net.IP(b.A[:]).String(); got != "192.0.2.10" {
				t.Errorf("A = %q, want 192.0.2.10", got)
			}
		case *dnsmessage.AAAAResource:
			gotAAAA = true
			if got := net.IP(b.AAAA[:]).String(); got != "2001:db8::1" {
				t.Errorf("AAAA = %q, want 2001:db8::1", got)
			}
		}
	}
	if !gotPTR || !gotSRV || !gotTXT || !gotA || !gotAAAA {
		t.Errorf("missing a record: PTR=%v SRV=%v TXT=%v A=%v AAAA=%v", gotPTR, gotSRV, gotTXT, gotA, gotAAAA)
	}

	// Hygiene: the only labels on the wire are the fixed, non-identifying ones.
	// A real host or site name would both fail make hygiene and leak onto a
	// broadcast medium.
	for _, name := range []string{ServiceName(), InstanceName(), hostLabel} {
		if !strings.Contains(name, "heyarr") {
			t.Errorf("advertised name %q is not the fixed non-identifying label", name)
		}
	}
}
