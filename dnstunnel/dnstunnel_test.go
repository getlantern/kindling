package dnstunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	mrand "math/rand/v2"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrameRoundTrip(t *testing.T) {
	f := &frame{kind: kindData, streamID: 0xBEEF, hasStream: true, seq: 0x01234567, hasSeq: true, payload: []byte("hello")}
	got, err := decodeFrame(f.encode())
	require.NoError(t, err)
	assert.Equal(t, f, got)

	bare, err := decodeFrame((&frame{kind: kindFin}).encode())
	require.NoError(t, err)
	assert.Equal(t, kindFin, bare.kind)
	assert.False(t, bare.hasStream)
}

func TestFrameDecodeRejectsMalformed(t *testing.T) {
	for _, b := range [][]byte{
		{},
		{9, byte(kindData), 0},               // bad version
		{frameVersion, 250, 0},               // unknown kind
		{frameVersion, byte(kindData), 0x80}, // unknown flag
		{frameVersion, byte(kindData), flagSeq, 0, 0}, // truncated seq
	} {
		_, err := decodeFrame(b)
		assert.Error(t, err, "%x", b)
	}
}

func TestSealOpenAndTamper(t *testing.T) {
	key := bytes.Repeat([]byte{0x5A}, keyLen)
	aead, err := newAEAD(ChaCha20Poly1305, key)
	require.NoError(t, err)
	id, _ := randomConnID()
	f := &frame{kind: kindData, streamID: 7, hasStream: true, seq: 42, hasSeq: true, payload: []byte("payload")}
	wire, err := sealShort(aead, id, f)
	require.NoError(t, err)

	p, err := parsePacket(wire)
	require.NoError(t, err)
	assert.Equal(t, id, p.connID)
	got, err := openFrame(aead, p.nonce, p.ciphertext)
	require.NoError(t, err)
	assert.Equal(t, f.payload, got.payload)

	wire[len(wire)-1] ^= 1
	p, err = parsePacket(wire)
	require.NoError(t, err)
	_, err = openFrame(aead, p.nonce, p.ciphertext)
	assert.Error(t, err, "tampered ciphertext must not open")
}

func TestParsersNeverPanic(t *testing.T) {
	r := mrand.New(mrand.NewPCG(1, 2))
	for n := range 400 {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		decodeFrame(b)
		parsePacket(b)
		parseAnswer(b)
	}
}

func TestBase32MatchesRFC4648(t *testing.T) {
	// RFC 4648 §10 vectors, lower-cased and unpadded.
	for in, want := range map[string]string{"": "", "f": "my", "fo": "mzxq", "foo": "mzxw6", "foob": "mzxw6yq", "fooba": "mzxw6ytb", "foobar": "mzxw6ytboi"} {
		assert.Equal(t, want, string(b32Encode([]byte(in))), in)
	}
}

func TestQueryFitsNameLimitAtUplinkSegment(t *testing.T) {
	zone, err := parseZone("t.example.com")
	require.NoError(t, err)
	seg := uplinkSegment(zone)
	require.Greater(t, seg, 50)
	aead, _ := newAEAD(ChaCha20Poly1305, make([]byte, keyLen))
	id, _ := randomConnID()
	payload := make([]byte, seg)
	rand.Read(payload)
	wire, err := sealShort(aead, id, &frame{kind: kindData, streamID: 0xFFFF, hasStream: true, seq: 1, hasSeq: true, payload: payload})
	require.NoError(t, err)
	_, err = buildQuery(1, wire, zone, 1232)
	assert.NoError(t, err, "a full uplink segment must fit the 255-byte QNAME")
}

func TestEncodeTarget(t *testing.T) {
	b, err := encodeTarget("example.com:443")
	require.NoError(t, err)
	assert.Equal(t, append(append([]byte{atypDomain, 11}, "example.com"...), 0x01, 0xBB), b)
	b, err = encodeTarget("1.2.3.4:80")
	require.NoError(t, err)
	assert.Equal(t, []byte{atypIPv4, 1, 2, 3, 4, 0, 80}, b)
	b, err = encodeTarget("[::1]:53")
	require.NoError(t, err)
	assert.Equal(t, byte(atypIPv6), b[0])
	assert.Len(t, b, 19)
}

func TestParseResolvers(t *testing.T) {
	p, err := parseResolvers([]string{"1.1.1.1", "8.8.8.8:5353", "[2001:db8::1]:53", "10.0.0.0/30", "1.1.1.1"}, 3)
	require.NoError(t, err)
	assert.Len(t, p.rs, 7, "dedup, default port, CIDR expansion")
	assert.Equal(t, netip.MustParseAddrPort("8.8.8.8:5353"), p.rs[1].addr)

	_, err = parseResolvers([]string{"10.0.0.0/8"}, 1)
	require.NoError(t, err)
	big, _ := parseResolvers([]string{"10.0.0.0/8"}, 1)
	assert.Len(t, big.rs, maxPoolHosts, "CIDR expansion is capped")

	_, err = parseResolvers(nil, 1)
	assert.Error(t, err)
}

func TestPoolFailsOverFromADeadStickyResolver(t *testing.T) {
	p, err := parseResolvers([]string{"192.0.2.1", "192.0.2.2"}, 1)
	require.NoError(t, err)
	dead := p.pick(0)[0]
	for i := range failoverStreak {
		p.onLoss([]netip.AddrPort{dead}, uint64(i))
	}
	assert.NotEqual(t, dead, p.pick(10)[0], "sticky resolver moves after a loss streak")
}

// TestARQOverLossyChannel drives two ARQ streams through a channel that drops, duplicates and
// reorders, and checks reliable in-order delivery plus a clean close.
func TestARQOverLossyChannel(t *testing.T) {
	cfg := arqConfig{maxSegment: 100, sendWindow: 32, recvWindow: 128, initialRTO: 50, minRTO: 20, maxRTO: 2000}
	a, b := newARQStream(1, cfg), newARQStream(1, cfg)
	r := mrand.New(mrand.NewPCG(7, 9))
	data := make([]byte, 50_000)
	rand.Read(data)
	a.write(data)
	a.close()

	type inflight struct {
		to  *arqStream
		f   *frame
		due uint64
	}
	var q []inflight
	send := func(to *arqStream, f *frame, now uint64) {
		if r.Float64() < 0.2 { // 20% loss
			return
		}
		copies := 1
		if r.Float64() < 0.1 { // 10% duplication
			copies = 2
		}
		for range copies {
			q = append(q, inflight{to, f, now + 5 + uint64(r.IntN(40))}) // reordering jitter
		}
	}
	var got []byte
	for now := uint64(0); now < 600_000; now += 5 {
		for f := a.pollTransmit(now); f != nil; f = a.pollTransmit(now) {
			send(b, f, now)
		}
		for f := b.pollTransmit(now); f != nil; f = b.pollTransmit(now) {
			send(a, f, now)
		}
		rest := q[:0]
		for _, m := range q {
			if m.due <= now {
				m.to.onFrame(m.f, now)
			} else {
				rest = append(rest, m)
			}
		}
		q = rest
		got = append(got, b.read()...)
		if b.remoteFinRecvd && !b.closing {
			b.close()
		}
		if a.isClosed() && b.isClosed() {
			break
		}
	}
	assert.Equal(t, data, got, "reliable, ordered delivery")
	assert.True(t, a.isClosed() && b.isClosed(), "both halves close")
}

func TestARQBoundsUndeliveredBytes(t *testing.T) {
	cfg := arqConfig{maxSegment: 100, sendWindow: 1000, recvWindow: 1000, maxDelivered: 1000, initialRTO: 1000, minRTO: 200, maxRTO: 30_000}
	s := newARQStream(1, cfg)
	for seq := range uint32(50) {
		s.onFrame(&frame{kind: kindData, hasStream: true, streamID: 1, seq: seq, hasSeq: true, payload: make([]byte, 100)}, 0)
	}
	assert.LessOrEqual(t, len(s.delivered), cfg.maxDelivered+cfg.maxSegment, "an app that stops reading can't grow the buffer without bound")
	before := s.rcvNxt
	s.read()
	s.onFrame(&frame{kind: kindData, hasStream: true, streamID: 1, seq: before, hasSeq: true, payload: []byte("x")}, 0)
	assert.Equal(t, before+1, s.rcvNxt, "delivery resumes once the app reads")
}

// A packet from an address the query wasn't sent to, a duplicate answer, or an unknown txn must not
// free query slots or credit the pool: the answer path is a trust boundary.
func TestAnswersOnlyAcceptedFromQueriedResolvers(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	zone, _ := parseZone("t.example.com")
	cfg := Config{Resolvers: []string{"192.0.2.1"}}
	cfg.setDefaults()
	sess, err := newClientSession(pub, zone, &cfg)
	require.NoError(t, err)
	pool, err := parseResolvers([]string{"192.0.2.1", "192.0.2.2"}, 1)
	require.NoError(t, err)
	p := &pump{cfg: &cfg, sess: sess, pool: pool, log: cfg.Logger, pending: map[uint16]*sentQuery{},
		pendOpen: map[uint16]*openReq{}, conns: map[uint16]*tunnelConn{}, closing: map[uint16]uint64{},
		estCh: make(chan struct{})}

	q := sess.pollQuery(0)
	require.NotNil(t, q)
	txn, _ := txnOf(q)
	target := netip.MustParseAddrPort("192.0.2.1:53")
	p.pending[txn] = &sentQuery{targets: []netip.AddrPort{target}, answered: []bool{false}}
	answer := append([]byte{q[0], q[1], 0x84, 0}, q[4:]...) // echo shape is enough: it parses

	p.handleAnswer(answerMsg{from: netip.MustParseAddrPort("203.0.113.9:53"), body: answer}, 1)
	assert.Contains(t, p.pending, txn, "spoofed source must be ignored")
	assert.Contains(t, sess.outstanding, txn, "spoofed source must not free the query slot")
	assert.False(t, pool.rs[0].hasRTT)

	p.handleAnswer(answerMsg{from: target, body: answer}, 1)
	assert.NotContains(t, p.pending, txn)
	assert.NotContains(t, sess.outstanding, txn)

	sess.outstanding[txn] = 99
	p.handleAnswer(answerMsg{from: target, body: answer}, 2)
	assert.Contains(t, sess.outstanding, txn, "a replayed answer for a finished query is ignored")
}
