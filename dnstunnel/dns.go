package dnstunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
)

const (
	typeTXT       = 16
	typeOPT       = 41
	classIN       = 1
	maxNameLen    = 255
	maxLabelLen   = 63
	dnsHeaderLen  = 12
	b32Alphabet   = "abcdefghijklmnopqrstuvwxyz234567"
	errNameTooLng = "dnstunnel: DNS name too long"
)

var (
	errDNSTruncated = errors.New("dnstunnel: DNS message truncated")
	errBadPointer   = errors.New("dnstunnel: bad name compression pointer")
)

// zoneName is the tunnel zone as labels.
type zoneName [][]byte

func parseZone(s string) (zoneName, error) {
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return nil, errors.New("dnstunnel: empty zone")
	}
	var z zoneName
	for _, part := range strings.Split(s, ".") {
		if part == "" || len(part) > maxLabelLen {
			return nil, errors.New(errNameTooLng)
		}
		z = append(z, []byte(part))
	}
	return z, nil
}

// wireLen is the zone's encoded length including the terminating zero.
func (z zoneName) wireLen() int {
	n := 1
	for _, l := range z {
		n += 1 + len(l)
	}
	return n
}

// b32Encode is RFC 4648 base32, lower-case, unpadded — DNS-label safe.
func b32Encode(data []byte) []byte {
	out := make([]byte, 0, (len(data)*8+4)/5)
	var acc uint64
	var bits uint
	for _, b := range data {
		acc = acc<<8 | uint64(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, b32Alphabet[(acc>>bits)&0x1f])
		}
	}
	if bits > 0 {
		out = append(out, b32Alphabet[(acc<<(5-bits))&0x1f])
	}
	return out
}

// buildQuery packs data base32 into the QNAME under zone as a TXT query with an EDNS0 OPT.
func buildQuery(txn uint16, data []byte, zone zoneName, ednsUDP uint16) ([]byte, error) {
	enc := b32Encode(data)
	nameLen := zone.wireLen()
	for i := 0; i < len(enc); i += maxLabelLen {
		nameLen += 1 + min(maxLabelLen, len(enc)-i)
	}
	if nameLen > maxNameLen {
		return nil, errors.New(errNameTooLng)
	}
	msg := make([]byte, 0, dnsHeaderLen+nameLen+4+11)
	msg = binary.BigEndian.AppendUint16(msg, txn)
	// Flags RD=1; QD=1, AN=0, NS=0, AR=1 (OPT).
	msg = append(msg, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 1)
	for i := 0; i < len(enc); i += maxLabelLen {
		l := enc[i:min(i+maxLabelLen, len(enc))]
		msg = append(msg, byte(len(l)))
		msg = append(msg, l...)
	}
	for _, l := range zone {
		msg = append(msg, byte(len(l)))
		msg = append(msg, l...)
	}
	msg = append(msg, 0)
	msg = binary.BigEndian.AppendUint16(msg, typeTXT)
	msg = binary.BigEndian.AppendUint16(msg, classIN)
	return appendOPT(msg, ednsUDP), nil
}

func appendOPT(msg []byte, ednsUDP uint16) []byte {
	msg = append(msg, 0)
	msg = binary.BigEndian.AppendUint16(msg, typeOPT)
	msg = binary.BigEndian.AppendUint16(msg, ednsUDP)
	return append(msg, 0, 0, 0, 0, 0, 0)
}

// parseAnswer returns the txn id and the concatenated character-strings of every TXT answer.
func parseAnswer(b []byte) (uint16, []byte, error) {
	r := dnsReader{buf: b}
	txn, err := r.u16()
	if err != nil {
		return 0, nil, err
	}
	if err := r.skip(2); err != nil {
		return 0, nil, err
	}
	qd, _ := r.u16()
	an, err := r.u16()
	if err != nil {
		return 0, nil, err
	}
	if err := r.skip(4); err != nil {
		return 0, nil, err
	}
	for range qd {
		if err := r.skipName(); err != nil {
			return 0, nil, err
		}
		if err := r.skip(4); err != nil {
			return 0, nil, err
		}
	}
	var data []byte
	for range an {
		if err := r.skipName(); err != nil {
			return 0, nil, err
		}
		rtype, _ := r.u16()
		if err := r.skip(6); err != nil {
			return 0, nil, err
		}
		rdlen, err := r.u16()
		if err != nil {
			return 0, nil, err
		}
		rdata, err := r.take(int(rdlen))
		if err != nil {
			return 0, nil, err
		}
		if rtype != typeTXT {
			continue
		}
		for i := 0; i < len(rdata); {
			n := int(rdata[i])
			i++
			if i+n > len(rdata) {
				return 0, nil, errDNSTruncated
			}
			data = append(data, rdata[i:i+n]...)
			i += n
		}
	}
	return txn, data, nil
}

// txnOf reads a DNS message's transaction id.
func txnOf(b []byte) (uint16, bool) {
	if len(b) < 2 {
		return 0, false
	}
	return binary.BigEndian.Uint16(b), true
}

type dnsReader struct {
	buf []byte
	pos int
}

func (r *dnsReader) take(n int) ([]byte, error) {
	if n < 0 || r.pos+n > len(r.buf) {
		return nil, errDNSTruncated
	}
	s := r.buf[r.pos : r.pos+n]
	r.pos += n
	return s, nil
}

func (r *dnsReader) skip(n int) error {
	_, err := r.take(n)
	return err
}

func (r *dnsReader) u16() (uint16, error) {
	b, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

// skipName skips a name; a compression pointer terminates it.
func (r *dnsReader) skipName() error {
	for {
		b, err := r.take(1)
		if err != nil {
			return err
		}
		switch b[0] & 0xC0 {
		case 0x00:
			if b[0] == 0 {
				return nil
			}
			if err := r.skip(int(b[0])); err != nil {
				return err
			}
		case 0xC0:
			return r.skip(1)
		default:
			return errBadPointer
		}
	}
}

// questionOf returns a message's first question (QNAME, QTYPE, QCLASS) as raw bytes.
func questionOf(msg []byte) []byte {
	r := dnsReader{buf: msg, pos: dnsHeaderLen}
	if len(msg) < dnsHeaderLen || r.skipName() != nil || r.skip(4) != nil {
		return nil
	}
	return msg[dnsHeaderLen:r.pos]
}

// answersQuestion reports whether msg is a DNS response to exactly the question q, compared
// case-insensitively since resolvers may randomize the case of a forwarded name (0x20).
func answersQuestion(msg, q []byte) bool {
	if len(msg) < dnsHeaderLen || msg[2]&0x80 == 0 || binary.BigEndian.Uint16(msg[4:6]) != 1 {
		return false
	}
	got := questionOf(msg)
	return got != nil && bytes.EqualFold(got, q)
}
