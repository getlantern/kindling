package dnstunnel

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
)

// Wire forms (first byte of a packet).
const (
	formShort  = 0x00 // FORM_SHORT ‖ conn_id ‖ nonce ‖ AEAD(inner)
	formSyn    = 0x01 // FORM_SYN ‖ conn_id ‖ client_eph
	formSynAck = 0x02 // FORM_SYNACK ‖ conn_id ‖ server_eph ‖ sig
)

const frameVersion = 1

// Inner-header flags: which optional fields are present.
const (
	flagStream     = 0b0000_0001
	flagSeq        = 0b0000_0010
	flagFragment   = 0b0000_0100
	flagCompressed = 0b0000_1000
	flagKnown      = flagStream | flagSeq | flagFragment | flagCompressed
)

type kind uint8

const (
	kindSyn          kind = 1
	kindSynAck       kind = 2
	kindData         kind = 3
	kindAck          kind = 4
	kindNack         kind = 5
	kindFin          kind = 6
	kindRst          kind = 7
	kindKeepAlive    kind = 8
	kindMtuProbe     kind = 9
	kindMtuProbeResp kind = 10
	kindSetMtu       kind = 11
)

var errTruncated = errors.New("dnstunnel: frame truncated")

// frame is the inner (AEAD-sealed) frame.
type frame struct {
	kind      kind
	streamID  uint16
	hasStream bool
	seq       uint32
	hasSeq    bool
	payload   []byte
}

func (f *frame) encode() []byte {
	var flags byte
	if f.hasStream {
		flags |= flagStream
	}
	if f.hasSeq {
		flags |= flagSeq
	}
	out := make([]byte, 0, 3+6+len(f.payload)+tagLen)
	out = append(out, frameVersion, byte(f.kind), flags)
	if f.hasStream {
		out = binary.BigEndian.AppendUint16(out, f.streamID)
	}
	if f.hasSeq {
		out = binary.BigEndian.AppendUint32(out, f.seq)
	}
	return append(out, f.payload...)
}

func decodeFrame(b []byte) (*frame, error) {
	if len(b) < 3 {
		return nil, errTruncated
	}
	if b[0] != frameVersion {
		return nil, fmt.Errorf("dnstunnel: unsupported frame version %d", b[0])
	}
	k := kind(b[1])
	if k < kindSyn || k > kindSetMtu {
		return nil, fmt.Errorf("dnstunnel: unknown packet kind %d", b[1])
	}
	flags := b[2]
	if flags&^flagKnown != 0 {
		return nil, fmt.Errorf("dnstunnel: unknown header flags %#08b", flags)
	}
	f := &frame{kind: k}
	p := b[3:]
	if flags&flagStream != 0 {
		if len(p) < 2 {
			return nil, errTruncated
		}
		f.streamID, f.hasStream, p = binary.BigEndian.Uint16(p), true, p[2:]
	}
	if flags&flagSeq != 0 {
		if len(p) < 4 {
			return nil, errTruncated
		}
		f.seq, f.hasSeq, p = binary.BigEndian.Uint32(p), true, p[4:]
	}
	// Neither side fragments or compresses in this protocol version's live paths; reject rather than
	// misread a payload whose meaning we don't implement.
	if flags&(flagFragment|flagCompressed) != 0 {
		return nil, errors.New("dnstunnel: fragmented or compressed frames are not supported")
	}
	f.payload = p
	return f, nil
}

func buildSyn(connID [connIDLen]byte, clientEph []byte) []byte {
	w := make([]byte, 0, 1+connIDLen+x25519PubLen)
	w = append(w, formSyn)
	w = append(w, connID[:]...)
	return append(w, clientEph...)
}

// sealShort seals f into FORM_SHORT ‖ conn_id ‖ nonce ‖ AEAD(inner), with empty AAD.
func sealShort(aead cipher.AEAD, connID [connIDLen]byte, f *frame) ([]byte, error) {
	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	inner := f.encode()
	w := make([]byte, 0, 1+connIDLen+nonceLen+len(inner)+tagLen)
	w = append(w, formShort)
	w = append(w, connID[:]...)
	w = append(w, nonce[:]...)
	return aead.Seal(w, nonce[:], inner, nil), nil
}

// packet is a parsed wire packet. Only the fields for its form are set.
type packet struct {
	form       byte
	connID     [connIDLen]byte
	serverEph  []byte
	sig        []byte
	nonce      []byte
	ciphertext []byte
}

func parsePacket(b []byte) (*packet, error) {
	if len(b) < 1+connIDLen {
		return nil, errTruncated
	}
	p := &packet{form: b[0]}
	copy(p.connID[:], b[1:1+connIDLen])
	rest := b[1+connIDLen:]
	switch p.form {
	case formSynAck:
		if len(rest) < x25519PubLen+ed25519SigLen {
			return nil, errTruncated
		}
		p.serverEph, p.sig = rest[:x25519PubLen], rest[x25519PubLen:x25519PubLen+ed25519SigLen]
	case formShort:
		if len(rest) < nonceLen+tagLen {
			return nil, errTruncated
		}
		p.nonce, p.ciphertext = rest[:nonceLen], rest[nonceLen:]
	case formSyn:
		if len(rest) < x25519PubLen {
			return nil, errTruncated
		}
	default:
		return nil, fmt.Errorf("dnstunnel: bad wire form %d", p.form)
	}
	return p, nil
}

func openFrame(aead cipher.AEAD, nonce, ciphertext []byte) (*frame, error) {
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	return decodeFrame(plain)
}
