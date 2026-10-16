package discovery

import (
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
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
// and reads the packet back, asserting the DNS-SD quartet is correct for each
// service a profile can announce as, and that nothing host- or
// person-identifying is on the wire.
func TestMessageContents(t *testing.T) {
	tests := []struct {
		name         string
		service      Service
		wantService  string
		wantInstance string
		wantHost     string
	}{
		{
			// A Params that names no service is heyarr's: callers that predate the
			// personal profile keep announcing exactly what they always did.
			name:         "zero service is the media service",
			wantService:  "_heyarr._tcp.local.",
			wantInstance: "heyarr._heyarr._tcp.local.",
			wantHost:     "heyarr.local.",
		},
		{
			name:         "media profile announces heyarr",
			service:      ServiceMedia,
			wantService:  "_heyarr._tcp.local.",
			wantInstance: "heyarr._heyarr._tcp.local.",
			wantHost:     "heyarr.local.",
		},
		{
			name:         "personal profile announces mnemosyne",
			service:      ServicePersonal,
			wantService:  "_mnemosyne._tcp.local.",
			wantInstance: "mnemosyne._mnemosyne._tcp.local.",
			wantHost:     "mnemosyne.local.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Params{Service: tt.service, Port: 7777, TLS: true, APIPath: "/api/v1"}
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
					if got := ans.Header.Name.String(); got != tt.wantService {
						t.Errorf("PTR owner = %q, want %q", got, tt.wantService)
					}
					if got := b.PTR.String(); got != tt.wantInstance {
						t.Errorf("PTR target = %q, want %q", got, tt.wantInstance)
					}
				case *dnsmessage.SRVResource:
					gotSRV = true
					if got := ans.Header.Name.String(); got != tt.wantInstance {
						t.Errorf("SRV owner = %q, want %q", got, tt.wantInstance)
					}
					if b.Port != 7777 {
						t.Errorf("SRV port = %d, want 7777", b.Port)
					}
					if got := b.Target.String(); got != tt.wantHost {
						t.Errorf("SRV target = %q, want %q", got, tt.wantHost)
					}
				case *dnsmessage.TXTResource:
					gotTXT = true
					if got := ans.Header.Name.String(); got != tt.wantInstance {
						t.Errorf("TXT owner = %q, want %q", got, tt.wantInstance)
					}
					if !reflect.DeepEqual(b.TXT, p.TXT()) {
						t.Errorf("TXT = %v, want %v", b.TXT, p.TXT())
					}
				case *dnsmessage.AResource:
					gotA = true
					if got := ans.Header.Name.String(); got != tt.wantHost {
						t.Errorf("A owner = %q, want %q", got, tt.wantHost)
					}
					if got := net.IP(b.A[:]).String(); got != "192.0.2.10" {
						t.Errorf("A = %q, want 192.0.2.10", got)
					}
				case *dnsmessage.AAAAResource:
					gotAAAA = true
					if got := ans.Header.Name.String(); got != tt.wantHost {
						t.Errorf("AAAA owner = %q, want %q", got, tt.wantHost)
					}
					if got := net.IP(b.AAAA[:]).String(); got != "2001:db8::1" {
						t.Errorf("AAAA = %q, want 2001:db8::1", got)
					}
				}
			}
			if !gotPTR || !gotSRV || !gotTXT || !gotA || !gotAAAA {
				t.Errorf("missing a record: PTR=%v SRV=%v TXT=%v A=%v AAAA=%v", gotPTR, gotSRV, gotTXT, gotA, gotAAAA)
			}

			// Hygiene: the only labels on the wire are the fixed, non-identifying
			// ones. A real host or site name would both fail make hygiene and leak
			// onto a broadcast medium.
			svc := p.service()
			for _, name := range []string{svc.Name(), svc.InstanceName(), svc.HostName()} {
				if !strings.Contains(name, svc.Label) {
					t.Errorf("advertised name %q is not the fixed non-identifying label %q", name, svc.Label)
				}
			}
		})
	}
}

// TestPersonalServiceIsInvisibleToHeyarrClients is the whole point of the
// personal service type (#758): a heyarr client browses `_heyarr._tcp` and
// must never be offered a Mnemosyne node, whose API surface and token
// namespace are different (ADR-0107). No name a Mnemosyne node puts on the
// wire may match the heyarr service type or carry the heyarr label.
func TestPersonalServiceIsInvisibleToHeyarrClients(t *testing.T) {
	if ServicePersonal.Type == ServiceMedia.Type {
		t.Fatal("the personal and media services share a DNS-SD type")
	}
	for _, name := range []string{ServicePersonal.Name(), ServicePersonal.InstanceName(), ServicePersonal.HostName()} {
		if strings.Contains(name, "heyarr") {
			t.Errorf("personal-profile name %q would match a heyarr client's browse", name)
		}
		if !strings.HasSuffix(name, "."+Domain) {
			t.Errorf("personal-profile name %q is not under %q", name, Domain)
		}
	}
	if got, want := ServicePersonal.Name(), "_mnemosyne._tcp.local."; got != want {
		t.Errorf("ServicePersonal.Name() = %q, want %q", got, want)
	}
	if got, want := ServiceName(), ServiceMedia.Name(); got != want {
		t.Errorf("package-level ServiceName() = %q, want the media default %q", got, want)
	}
	if got, want := InstanceName(), ServiceMedia.InstanceName(); got != want {
		t.Errorf("package-level InstanceName() = %q, want the media default %q", got, want)
	}
}

// TestMediaMessageIsByteIdentical pins the media announcement to the exact
// packet the node sent before the personal service type existed (#758). The
// media profile is the deployed one; adding a second service must not change a
// single byte of what existing heyarr clients already parse. The golden is the
// hex of Message() for the inputs below, captured from the pre-change code.
func TestMediaMessageIsByteIdentical(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "media-announcement.hex"))
	if err != nil {
		t.Fatal(err)
	}
	addrs := []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::1")}
	for _, tt := range []struct {
		name string
		p    Params
	}{
		{name: "zero service", p: Params{Port: 7777, TLS: true, APIPath: "/api/v1"}},
		{name: "explicit media service", p: Params{Service: ServiceMedia, Port: 7777, TLS: true, APIPath: "/api/v1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			packed, err := tt.p.Message(addrs, 0)
			if err != nil {
				t.Fatalf("Message: %v", err)
			}
			if got := hex.EncodeToString(packed); got != strings.TrimSpace(string(want)) {
				t.Errorf("media announcement bytes changed:\n got %s\nwant %s", got, strings.TrimSpace(string(want)))
			}
		})
	}
}
