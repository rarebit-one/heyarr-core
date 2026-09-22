// Package yubikey is the ADR-0098 "YubiKey on-card" device-key custody backend:
// a personalstate/client.Unwrapper whose AgreementFunc drives the OpenPGP
// applet's cv25519 PSO:DECIPHER on the token, so heyarr's X25519 encryption key
// never leaves the card. It talks to the running gpg-agent/scdaemon over Assuan
// and sends raw card APDUs, rather than grabbing the reader over PC/SC, so it
// coexists with the user's gpg (which owns the card) instead of contending for it.
package yubikey

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// scd is a minimal Assuan client for the gpg-agent socket. The agent proxies
// SCD commands to scdaemon; we use SCD READKEY (public key) and SCD APDU (raw
// PSO:DECIPHER / VERIFY). It is not safe for concurrent use — one unwrap at a time.
type scd struct {
	conn net.Conn
	r    *bufio.Reader
}

// agentSocket returns the gpg-agent Assuan socket, from gpgconf when path is "".
func agentSocket(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	out, err := exec.Command("gpgconf", "--list-dirs", "agent-socket").Output()
	if err != nil {
		return "", fmt.Errorf("yubikey: locating gpg-agent socket: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// dialSCD connects to the agent and consumes its greeting.
func dialSCD(socket string) (*scd, error) {
	sock, err := agentSocket(socket)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("yubikey: dialling gpg-agent: %w", err)
	}
	s := &scd{conn: conn, r: bufio.NewReader(conn)}
	if _, err := s.readReply(); err != nil { // greeting
		_ = conn.Close()
		return nil, fmt.Errorf("yubikey: gpg-agent greeting: %w", err)
	}
	return s, nil
}

func (s *scd) close() error { return s.conn.Close() }

// readReply reads Assuan response lines until OK/ERR, collecting D data.
func (s *scd) readReply() ([]byte, error) {
	var data []byte
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "D "):
			data = append(data, unescape([]byte(line[2:]))...)
		case line == "OK" || strings.HasPrefix(line, "OK "):
			return data, nil
		case strings.HasPrefix(line, "ERR"):
			return nil, fmt.Errorf("yubikey: gpg-agent: %s", line)
		default:
			// S (status), # (comment), INQUIRE — none expected for our commands.
		}
	}
}

// cmd sends one Assuan command and returns its collected D data.
func (s *scd) cmd(line string) ([]byte, error) {
	if _, err := s.conn.Write([]byte(line + "\n")); err != nil {
		return nil, err
	}
	return s.readReply()
}

// apdu sends a raw APDU (hex) via scdaemon and returns the response, SW included.
func (s *scd) apdu(hexAPDU string) ([]byte, error) {
	return s.cmd("SCD APDU " + hexAPDU)
}

// unescape decodes Assuan percent-escaping (%XX and %%) in a D line.
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '%' && i+2 < len(b) {
			var v byte
			if _, err := fmt.Sscanf(string(b[i+1:i+3]), "%02x", &v); err == nil {
				out = append(out, v)
				i += 2
				continue
			}
		}
		out = append(out, b[i])
	}
	return out
}

// parsePubkeyPoint extracts the 32-byte X25519 point from a SCD READKEY
// canonical S-expression: (public-key(ecc(curve Curve25519)(flags djb-tweak)
// (q33:@<32 bytes>))). The point is the 33-byte "q" with its 0x40 native-point
// prefix stripped.
func parsePubkeyPoint(sexp []byte) ([]byte, error) {
	const marker = "1:q33:"
	i := strings.Index(string(sexp), marker)
	if i < 0 {
		return nil, fmt.Errorf("yubikey: no 33-byte q in card public key")
	}
	q := sexp[i+len(marker):]
	if len(q) < 33 {
		return nil, fmt.Errorf("yubikey: truncated card public key")
	}
	if q[0] != 0x40 {
		return nil, fmt.Errorf("yubikey: card public key prefix %#x is not the 0x40 native point", q[0])
	}
	point := make([]byte, 32)
	copy(point, q[1:33])
	return point, nil
}

// decipherDO builds the PSO:DECIPHER command data for an X25519 agreement: the
// Cipher DO A6 { 7F49 { 86 <ephemeral point> } } (OpenPGP application spec §7.2.11).
func decipherDO(ephPub []byte) (string, error) {
	if len(ephPub) != 32 {
		return "", fmt.Errorf("yubikey: ephemeral public key is %d bytes, want 32", len(ephPub))
	}
	// 86 20 <32>  ->  7F49 22 <..>  ->  A6 25 <..>
	return "A6257F49228620" + hex.EncodeToString(ephPub), nil
}

// checkSW returns the response body with the trailing status word validated as
// 0x9000. resp includes SW1SW2.
func checkSW(resp []byte, what string) ([]byte, error) {
	if len(resp) < 2 {
		return nil, fmt.Errorf("yubikey: %s: short response (%d bytes)", what, len(resp))
	}
	sw := resp[len(resp)-2:]
	if sw[0] != 0x90 || sw[1] != 0x00 {
		return nil, fmt.Errorf("yubikey: %s: card returned SW %02x%02x", what, sw[0], sw[1])
	}
	return resp[:len(resp)-2], nil
}
