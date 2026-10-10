package dnstunnel

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"slices"
)

// clientSession is the sans-I/O client end of a tunnel session: a forward-secret handshake with the
// server, then many ARQ streams multiplexed over one key schedule. DNS is strictly request→response,
// so the client polls: every query carries one uplink frame (or a KeepAlive when idle, giving the
// server a chance to answer with downlink data).
type clientSession struct {
	zone       zoneName
	ednsUDP    uint16
	maxInfl    int
	queryTO    uint64
	cipher     Cipher
	connID     [connIDLen]byte
	serverPub  ed25519.PublicKey
	clientEph  *ecdh.PrivateKey
	up, down   cipher.AEAD
	upCfg      arqConfig
	streams    map[uint16]*clientStream
	nextStream uint16
	rrLast     uint16
	txn        uint16
	// outstanding maps txn → deadline for queries awaiting an answer.
	outstanding map[uint16]uint64
	// holdOpens defers stream opens (while the MTU probe settles), so every stream's first downlink
	// segment is cut at the probed size.
	holdOpens bool
	// holdData withholds stream data while a SetMtu is unacknowledged, so the server resizes a stream
	// before cutting its first downlink segment (it never re-cuts one already sent).
	holdData bool
}

type clientStream struct {
	arq        *arqStream
	target     []byte
	openAcked  bool
	lastOpenMS uint64
	opened     bool // the open Syn has been sent at least once
}

func newClientSession(serverPub ed25519.PublicKey, zone zoneName, cfg *Config) (*clientSession, error) {
	connID, err := randomConnID()
	if err != nil {
		return nil, err
	}
	eph, err := newEphemeral()
	if err != nil {
		return nil, err
	}
	up := cfg.arq
	up.maxSegment = uplinkSegment(zone)
	return &clientSession{
		zone:        zone,
		ednsUDP:     cfg.EDNSUDPSize,
		maxInfl:     cfg.MaxQueriesInFlight,
		queryTO:     uint64(cfg.QueryTimeout.Milliseconds()),
		cipher:      cfg.Cipher,
		connID:      connID,
		serverPub:   serverPub,
		clientEph:   eph,
		upCfg:       up,
		streams:     make(map[uint16]*clientStream),
		nextStream:  1,
		outstanding: make(map[uint16]uint64),
	}, nil
}

func (s *clientSession) established() bool { return s.up != nil }

// openStream registers a stream to target; its open Syn goes out once the session is established.
// maxStreams bounds concurrent streams, and with them per-stream buffers. Bootstrap needs a handful.
const maxStreams = 32

var errTooManyStreams = errors.New("dnstunnel: too many concurrent streams")

func (s *clientSession) openStream(target []byte) (uint16, error) {
	if len(s.streams) >= maxStreams {
		return 0, errTooManyStreams
	}
	// With at most maxStreams live, a free id is always within maxStreams+1 steps.
	sid := s.nextStream
	for sid == 0 || s.streams[sid] != nil {
		sid++
	}
	s.nextStream = sid + 1
	s.streams[sid] = &clientStream{arq: newARQStream(sid, s.upCfg), target: target}
	return sid, nil
}

func (s *clientSession) stream(sid uint16) *clientStream { return s.streams[sid] }

func (s *clientSession) sortedIDs() []uint16 {
	ids := make([]uint16, 0, len(s.streams))
	for id := range s.streams {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// reapClosed drops fully closed streams and returns their ids. A reset stream the server opened is
// kept until its RST has gone out, or the server would keep sending to it; one it never acknowledged
// has nothing to reset (and could never send the RST), so it goes at once.
func (s *clientSession) reapClosed() []uint16 {
	var out []uint16
	for id, st := range s.streams {
		if st.arq.isClosed() && (!st.arq.rstPending || !st.openAcked) {
			delete(s.streams, id)
			out = append(out, id)
		}
	}
	return out
}

func (s *clientSession) anyAlive() bool {
	for _, st := range s.streams {
		if !st.arq.isClosed() {
			return true
		}
	}
	return false
}

// pollQuery returns the next DNS query to send, or nil if the in-flight budget is full or there is
// nothing useful to send.
func (s *clientSession) pollQuery(now uint64) []byte {
	for txn, deadline := range s.outstanding {
		if deadline <= now {
			delete(s.outstanding, txn)
		}
	}
	if len(s.outstanding) >= s.maxInfl {
		return nil
	}
	var wire []byte
	switch {
	case !s.established():
		// Only the cleartext Syn before the handshake, re-sent whenever none is outstanding.
		if len(s.outstanding) > 0 {
			return nil
		}
		wire = buildSyn(s.connID, s.clientEph.PublicKey().Bytes())
	default:
		f := s.nextOpenSyn(now)
		if f == nil {
			f = s.nextStreamFrame(now)
		}
		if f == nil {
			if !s.anyAlive() {
				return nil
			}
			f = &frame{kind: kindKeepAlive}
		}
		w, err := sealShort(s.up, s.connID, f)
		if err != nil {
			return nil
		}
		wire = w
	}
	s.txn++
	q, err := buildQuery(s.txn, wire, s.zone, s.ednsUDP)
	if err != nil {
		return nil
	}
	s.outstanding[s.txn] = now + s.queryTO
	return q
}

// nextOpenSyn returns a stream-open Syn whose retransmit timer is due, if any.
func (s *clientSession) nextOpenSyn(now uint64) *frame {
	if s.holdOpens {
		return nil
	}
	for _, id := range s.sortedIDs() {
		st := s.streams[id]
		// A reset stream (a cancelled dial) must not be (re)opened on the server.
		if st.openAcked || st.arq.isClosed() || (st.opened && now-min(now, st.lastOpenMS) < s.upCfg.initialRTO) {
			continue
		}
		st.opened, st.lastOpenMS = true, now
		return &frame{kind: kindSyn, streamID: id, hasStream: true, payload: st.target}
	}
	return nil
}

// nextStreamFrame round-robins open streams so none starves the others under the shared budget.
func (s *clientSession) nextStreamFrame(now uint64) *frame {
	if s.holdData {
		return nil
	}
	ids := s.sortedIDs()
	if len(ids) == 0 {
		return nil
	}
	start := 0
	for i, id := range ids {
		if id > s.rrLast {
			start = i
			break
		}
	}
	for k := range ids {
		id := ids[(start+k)%len(ids)]
		st := s.streams[id]
		if !st.openAcked {
			continue
		}
		if f := st.arq.pollTransmit(now); f != nil {
			s.rrLast = id
			return f
		}
	}
	return nil
}

// onAnswer feeds a DNS answer into the session.
// onAnswer feeds a DNS answer into the session. It returns the payload size of an MTU probe that
// came back intact, or 0.
func (s *clientSession) onAnswer(msg []byte, now uint64) (probeOK uint16) {
	txn, data, err := parseAnswer(msg)
	if err != nil {
		return 0
	}
	delete(s.outstanding, txn)
	if len(data) == 0 {
		return 0
	}
	p, err := parsePacket(data)
	if err != nil || p.connID != s.connID {
		return 0
	}
	if !s.established() {
		if p.form == formSynAck {
			s.finishHandshake(p.serverEph, p.sig)
		}
		return 0
	}
	if p.form != formShort {
		return 0
	}
	f, err := openFrame(s.down, p.nonce, p.ciphertext)
	if err != nil {
		return 0
	}
	if f.kind == kindMtuProbeResp && len(f.payload) >= 2 {
		return binary.BigEndian.Uint16(f.payload)
	}
	if !f.hasStream {
		return 0
	}
	st := s.streams[f.streamID]
	if st == nil {
		return
	}
	if f.kind == kindSynAck {
		st.openAcked = true
		return 0
	}
	st.arq.onFrame(f, now)
	return 0
}

// controlQuery seals a session-level control frame (MTU probe, SetMtu) into a query. Control queries
// don't occupy the in-flight budget; the pump tracks them itself.
func (s *clientSession) controlQuery(k kind, size uint16) []byte {
	if !s.established() {
		return nil
	}
	w, err := sealShort(s.up, s.connID, &frame{kind: k, payload: binary.BigEndian.AppendUint16(nil, size)})
	if err != nil {
		return nil
	}
	s.txn++
	q, err := buildQuery(s.txn, w, s.zone, s.ednsUDP)
	if err != nil {
		return nil
	}
	return q
}

// finishHandshake verifies the server's transcript signature, agrees, and installs the session keys.
// On any failure the handshake stays incomplete and the Syn keeps being retried.
func (s *clientSession) finishHandshake(serverEph, sig []byte) {
	tr := transcript(s.clientEph.PublicKey().Bytes(), serverEph, s.connID)
	if verifyServerSig(s.serverPub, tr, sig) != nil {
		return
	}
	ee, err := agree(s.clientEph, serverEph)
	if err != nil {
		return
	}
	upKey, downKey, err := deriveSessionKeys(ee, tr)
	if err != nil {
		return
	}
	up, err := newAEAD(s.cipher, upKey)
	if err != nil {
		return
	}
	down, err := newAEAD(s.cipher, downKey)
	if err != nil {
		return
	}
	s.up, s.down, s.clientEph = up, down, nil
}
