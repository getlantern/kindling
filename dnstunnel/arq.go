package dnstunnel

// The ARQ: a reliable, ordered byte stream over the unreliable frame carrier. A sans-I/O state
// machine driven by a caller-supplied millisecond clock, wire-compatible with spark's
// dns-tunnel-core arq. Sequence numbers are per segment; the FIN occupies one phantom segment.
// There is deliberately no congestion control: the send window and the resolvers' own rate limits
// govern the rate.

type streamState int

const (
	stateOpen streamState = iota
	stateFinSent
	stateFinRcvd
	stateClosed
	stateReset
)

// seqLT reports a < b in 32-bit serial-number arithmetic (RFC 1982).
func seqLT(a, b uint32) bool { return int32(a-b) < 0 }

type arqConfig struct {
	maxSegment   int
	sendWindow   uint32
	recvWindow   uint32
	maxDelivered int // cap on undelivered in-order bytes; past it new data is dropped and re-sent
	initialRTO   uint64
	minRTO       uint64
	maxRTO       uint64
}

type sentSeg struct {
	data   []byte
	sentAt uint64
	retx   uint32
	isFin  bool
}

type arqStream struct {
	id  uint16
	cfg arqConfig

	state streamState

	sndUna, sndNxt uint32
	outbox         []byte
	inflight       map[uint32]*sentSeg
	fastRetx       []uint32
	closing        bool
	finSeq         *uint32
	localFinAcked  bool
	rstPending     bool

	rcvNxt         uint32
	reorder        map[uint32][]byte
	delivered      []byte
	remoteFinSeq   *uint32
	remoteFinRecvd bool

	ackPending  bool
	nackPending *uint32

	srtt   float64
	hasRTT bool
	rttvar float64
	rto    uint64
}

func newARQStream(id uint16, cfg arqConfig) *arqStream {
	return &arqStream{
		id:       id,
		cfg:      cfg,
		inflight: make(map[uint32]*sentSeg),
		reorder:  make(map[uint32][]byte),
		rto:      cfg.initialRTO,
	}
}

func (s *arqStream) isClosed() bool { return s.state == stateClosed || s.state == stateReset }

func (s *arqStream) write(b []byte) { s.outbox = append(s.outbox, b...) }

func (s *arqStream) close() { s.closing = true }

func (s *arqStream) reset() {
	s.state = stateReset
	s.rstPending = true
	s.dropBuffers()
}

func (s *arqStream) dropBuffers() {
	clear(s.inflight)
	clear(s.reorder)
	s.outbox = nil
	s.fastRetx = nil
}

// read drains the in-order delivered bytes.
func (s *arqStream) read() []byte {
	b := s.delivered
	s.delivered = nil
	return b
}

// unsent is the number of queued-but-unsent bytes, for writer backpressure.
func (s *arqStream) unsent() int { return len(s.outbox) }

func (s *arqStream) inflightCount() uint32 { return s.sndNxt - s.sndUna }

func (s *arqStream) onFrame(f *frame, now uint64) {
	switch f.kind {
	case kindData:
		if f.hasSeq {
			s.onData(f.seq, f.payload)
		}
	case kindAck:
		if f.hasSeq {
			s.onAck(f.seq, now)
		}
	case kindNack:
		if f.hasSeq {
			if _, ok := s.inflight[f.seq]; ok && !containsSeq(s.fastRetx, f.seq) {
				s.fastRetx = append(s.fastRetx, f.seq)
			}
		}
	case kindFin:
		if f.hasSeq {
			s.ackPending = true
			if s.remoteFinSeq == nil {
				seq := f.seq
				s.remoteFinSeq = &seq
			}
			s.advanceRecv()
		}
	case kindRst:
		s.state = stateReset
		s.dropBuffers()
	case kindKeepAlive:
		s.ackPending = true
	}
}

func containsSeq(q []uint32, seq uint32) bool {
	for _, v := range q {
		if v == seq {
			return true
		}
	}
	return false
}

func (s *arqStream) onData(seq uint32, payload []byte) {
	// Always ack, even duplicates and gaps, so the sender learns our cumulative position.
	s.ackPending = true
	if seqLT(seq, s.rcvNxt) || !seqLT(seq, s.rcvNxt+s.cfg.recvWindow) {
		return
	}
	// Bound memory: if the app isn't reading, refuse new data; the peer retransmits it later.
	if s.cfg.maxDelivered > 0 && len(s.delivered) >= s.cfg.maxDelivered {
		return
	}
	if seq == s.rcvNxt {
		s.delivered = append(s.delivered, payload...)
		s.rcvNxt++
		s.advanceRecv()
		return
	}
	if _, ok := s.reorder[seq]; !ok {
		// payload aliases the decrypted frame buffer, which is not reused, so no copy is needed.
		s.reorder[seq] = payload
	}
	gap := s.rcvNxt
	s.nackPending = &gap
}

// advanceRecv delivers contiguous buffered segments, then consumes the remote FIN at rcvNxt.
func (s *arqStream) advanceRecv() {
	// Promotion honors the delivery cap too; the pump resumes it once the app has read.
	for s.cfg.maxDelivered <= 0 || len(s.delivered) < s.cfg.maxDelivered {
		if next, ok := s.reorder[s.rcvNxt]; ok {
			delete(s.reorder, s.rcvNxt)
			s.delivered = append(s.delivered, next...)
			s.rcvNxt++
			continue
		}
		if s.remoteFinSeq != nil && *s.remoteFinSeq == s.rcvNxt && !s.remoteFinRecvd {
			s.rcvNxt++
			s.remoteFinRecvd = true
			s.updateState()
		}
		break
	}
	if len(s.reorder) == 0 {
		s.nackPending = nil
	} else {
		gap := s.rcvNxt
		s.nackPending = &gap
	}
}

// resumeRecv promotes buffered segments held back by the delivery cap, once the app has read, and
// acks the new position so the sender learns of it.
func (s *arqStream) resumeRecv() {
	if len(s.reorder) == 0 && (s.remoteFinSeq == nil || s.remoteFinRecvd) {
		return
	}
	before := s.rcvNxt
	s.advanceRecv()
	if s.rcvNxt != before {
		s.ackPending = true
	}
}

// onAck handles a cumulative ack: ack is the peer's next-expected seq.
func (s *arqStream) onAck(ack uint32, now uint64) {
	var rtt uint64
	sampled := false
	for seq, seg := range s.inflight {
		if !seqLT(seq, ack) {
			continue
		}
		// Karn: sample RTT only from never-retransmitted data segments.
		if seg.retx == 0 && !seg.isFin {
			rtt, sampled = now-min(now, seg.sentAt), true
		}
		delete(s.inflight, seq)
	}
	if seqLT(s.sndUna, ack) {
		s.sndUna = ack
	}
	if s.finSeq != nil && seqLT(*s.finSeq, ack) {
		s.localFinAcked = true
	}
	if sampled {
		s.updateRTO(float64(rtt))
	}
	s.updateState()
}

// updateRTO is the RFC 6298 estimator.
func (s *arqStream) updateRTO(r float64) {
	if !s.hasRTT {
		s.srtt, s.rttvar, s.hasRTT = r, r/2, true
	} else {
		d := s.srtt - r
		if d < 0 {
			d = -d
		}
		s.rttvar = 0.75*s.rttvar + 0.25*d
		s.srtt = 0.875*s.srtt + 0.125*r
	}
	rto := uint64(max(s.srtt+4*s.rttvar, 1))
	s.rto = min(max(rto, s.cfg.minRTO), s.cfg.maxRTO)
}

func (s *arqStream) updateState() {
	if s.state == stateReset {
		return
	}
	finSent := s.finSeq != nil
	switch {
	case finSent && s.localFinAcked && s.remoteFinRecvd:
		s.state = stateClosed
	case s.remoteFinRecvd:
		s.state = stateFinRcvd
	case finSent:
		s.state = stateFinSent
	default:
		s.state = stateOpen
	}
}

// pollTransmit returns the next frame to send at now, or nil. Priority: RST, NACK fast-retransmit,
// RTO retransmit, NACK, new data, FIN, standalone ACK.
func (s *arqStream) pollTransmit(now uint64) *frame {
	if s.rstPending {
		s.rstPending = false
		return &frame{kind: kindRst, streamID: s.id, hasStream: true}
	}
	if s.state == stateReset {
		return nil
	}
	if s.state == stateClosed {
		// Keep acking the peer's FIN retransmits so it can close too, but send nothing new.
		if s.ackPending {
			s.ackPending = false
			return s.ackFrame()
		}
		return nil
	}
	for len(s.fastRetx) > 0 {
		seq := s.fastRetx[0]
		s.fastRetx = s.fastRetx[1:]
		if seg, ok := s.inflight[seq]; ok {
			seg.sentAt = now
			seg.retx++
			return s.segFrame(seq, seg.data, seg.isFin)
		}
	}
	// Retransmit the lowest-seq segment whose RTO has expired.
	var due *sentSeg
	var dueSeq uint32
	for seq, seg := range s.inflight {
		if now >= seg.sentAt+s.rto && (due == nil || seqLT(seq, dueSeq)) {
			due, dueSeq = seg, seq
		}
	}
	if due != nil {
		due.sentAt = now
		due.retx++
		s.rto = min(s.rto*2, s.cfg.maxRTO)
		return s.segFrame(dueSeq, due.data, due.isFin)
	}
	if s.nackPending != nil {
		seq := *s.nackPending
		s.nackPending = nil
		return &frame{kind: kindNack, streamID: s.id, hasStream: true, seq: seq, hasSeq: true}
	}
	if s.inflightCount() < s.cfg.sendWindow && len(s.outbox) > 0 {
		n := min(len(s.outbox), s.cfg.maxSegment)
		data := make([]byte, n)
		copy(data, s.outbox)
		s.outbox = s.outbox[n:]
		if len(s.outbox) == 0 {
			s.outbox = nil // release the backing array once drained
		}
		seq := s.sndNxt
		s.sndNxt++
		s.inflight[seq] = &sentSeg{data: data, sentAt: now}
		return s.segFrame(seq, data, false)
	}
	if s.closing && s.finSeq == nil && len(s.outbox) == 0 && s.inflightCount() < s.cfg.sendWindow {
		seq := s.sndNxt
		s.sndNxt++
		s.finSeq = &seq
		s.inflight[seq] = &sentSeg{sentAt: now, isFin: true}
		s.updateState()
		return s.segFrame(seq, nil, true)
	}
	// Standalone acks go last so they never starve data under continuous traffic.
	if s.ackPending {
		s.ackPending = false
		return s.ackFrame()
	}
	return nil
}

func (s *arqStream) segFrame(seq uint32, data []byte, isFin bool) *frame {
	k := kindData
	if isFin {
		k = kindFin
	}
	return &frame{kind: k, streamID: s.id, hasStream: true, seq: seq, hasSeq: true, payload: data}
}

func (s *arqStream) ackFrame() *frame {
	return &frame{kind: kindAck, streamID: s.id, hasStream: true, seq: s.rcvNxt, hasSeq: true}
}
