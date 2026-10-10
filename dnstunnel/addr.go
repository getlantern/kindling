package dnstunnel

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// SOCKS5 address types used for a stream's target.
const (
	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04
)

// encodeTarget encodes host:port as SOCKS5 ATYP ‖ addr ‖ port. A domain stays a domain so the exit
// resolves it: a network that poisons DNS never sees the lookup.
func encodeTarget(hostport string) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, fmt.Errorf("dnstunnel: bad target %q: %w", hostport, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("dnstunnel: bad port in %q: %w", hostport, err)
	}
	var out []byte
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Is4() {
			b := ip.As4()
			out = append([]byte{atypIPv4}, b[:]...)
		} else {
			b := ip.As16()
			out = append([]byte{atypIPv6}, b[:]...)
		}
	} else {
		if host == "" || len(host) > 255 {
			return nil, fmt.Errorf("dnstunnel: domain length %d outside 1..255", len(host))
		}
		out = append([]byte{atypDomain, byte(len(host))}, host...)
	}
	return binary.BigEndian.AppendUint16(out, uint16(port)), nil
}
