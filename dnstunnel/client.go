// Package dnstunnel is a Go client for spark's DNS-tunnel protocol (spark ADR 0011), wire-compatible
// with spark's dns-tunnel-server. It carries TCP streams over DNS TXT queries through recursive
// resolvers, so it still works when nothing but DNS does.
//
// It is a last-resort bootstrap path, sized for small requests on memory-constrained platforms
// (notably the iOS network extension): one session at a time, created on first use and torn down
// once idle, with every buffer bounded.
package dnstunnel

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"
)

const (
	// Bounds on per-stream buffering, chosen for small bootstrap requests.
	connReadBuf = 64 << 10 // max bytes queued for the app to read
	maxUnsent   = 32 << 10 // max bytes queued for the tunnel per stream before Write blocks
	writeChunk  = 8 << 10
	udpReadBuf  = 4096 // EDNSUDPSize is clamped to this, so a full answer always fits
	answerQueue = 64
	pumpTick    = 20 * time.Millisecond
	sendTimeout = 100 * time.Millisecond
	// closeLinger bounds how long a stream the app closed may linger finishing its graceful close
	// before it is reset, so a dead peer can't pin the session open.
	closeLinger = 10_000
	// probeWindow is how long downlink MTU probes may take to return. Longer than spark's 400ms
	// because resolver round trips from censored networks often exceed it.
	probeWindow  = 1_500
	maxReqLength = 16 << 10
)

// Config configures a Client.
type Config struct {
	// Zone is the NS-delegated tunnel zone, e.g. "t.example.com".
	Zone string
	// ServerPublicKey is the server's static Ed25519 public key, base64. Not a secret.
	ServerPublicKey string
	// Resolvers are the recursive resolvers to spray queries across (IP, IP:port, [v6]:port, CIDR).
	// Empty uses /etc/resolv.conf where it exists. Mobile platforms have none and must set this.
	Resolvers []string
	// Duplication sends each query to this many resolvers at once. It trades bandwidth for finding
	// a working resolver fast on a mostly-dead pool. Default 3.
	Duplication int
	// Cipher is the session AEAD. Default ChaCha20Poly1305.
	Cipher Cipher
	// EDNSUDPSize is the EDNS0 payload size advertised on queries. Default 1232.
	EDNSUDPSize uint16
	// MaxQueriesInFlight bounds concurrent DNS queries, the main throughput and memory lever.
	// Default 16.
	MaxQueriesInFlight int
	// QueryTimeout frees an unanswered query's slot. Default 3s.
	QueryTimeout time.Duration
	// IdleTimeout tears the session down once it has had no streams this long. Default 3s.
	IdleTimeout time.Duration
	// ListenPacket opens the UDP socket the tunnel uses; set it to protect the socket from a VPN
	// route. Default net.ListenUDP on an unspecified address.
	ListenPacket func(ctx context.Context) (net.PacketConn, error)
	// Logger receives debug logs. Resolver addresses and the zone are never logged.
	Logger *slog.Logger

	arq arqConfig
}

func (cfg *Config) setDefaults() {
	if cfg.Duplication <= 0 {
		cfg.Duplication = 3
	}
	if cfg.EDNSUDPSize == 0 {
		cfg.EDNSUDPSize = 1232
	}
	cfg.EDNSUDPSize = min(max(cfg.EDNSUDPSize, 512), udpReadBuf)
	if cfg.MaxQueriesInFlight <= 0 {
		cfg.MaxQueriesInFlight = 16
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 3 * time.Second
	}
	// The protocol clock is in milliseconds; anything shorter would expire every query on sight.
	cfg.QueryTimeout = max(cfg.QueryTimeout, 50*time.Millisecond)
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 3 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.ListenPacket == nil {
		cfg.ListenPacket = func(ctx context.Context) (net.PacketConn, error) {
			var lc net.ListenConfig
			return lc.ListenPacket(ctx, "udp", ":0")
		}
	}
	// The send window must cover the query budget or it re-bottlenecks the pull model; the receive
	// window absorbs reordering across resolvers.
	cfg.arq = arqConfig{
		sendWindow:   uint32(max(32, cfg.MaxQueriesInFlight)),
		recvWindow:   128,
		maxDelivered: connReadBuf,
		initialRTO:   1000,
		minRTO:       200,
		maxRTO:       30_000,
	}
}

// Client dials streams over the DNS tunnel. All dials share one session, built on first use and
// torn down when idle. It satisfies kindling's DNS-tunnel transport interface.
type Client struct {
	cfg       Config
	zone      zoneName
	serverPub ed25519.PublicKey
	resolvers []string

	mu     sync.Mutex
	pump   *pump
	closed bool
}

// New validates cfg and returns a Client. No sockets are opened until the first dial.
func New(cfg Config) (*Client, error) {
	cfg.setDefaults()
	zone, err := parseZone(cfg.Zone)
	if err != nil {
		return nil, err
	}
	pub, err := decodeServerPub(cfg.ServerPublicKey)
	if err != nil {
		return nil, err
	}
	resolvers := cfg.Resolvers
	if len(resolvers) == 0 {
		resolvers = systemResolvers()
	}
	if _, err := parseResolvers(resolvers, cfg.Duplication); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, zone: zone, serverPub: pub, resolvers: resolvers}, nil
}

// DialContext opens a tunnel stream to addr (host:port). A domain is resolved by the tunnel exit,
// never locally. Only TCP is supported.
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("dnstunnel: unsupported network %q", network)
	}
	target, err := encodeTarget(addr)
	if err != nil {
		return nil, err
	}
	// The stream-open frame carries the target unfragmented, so it must fit one query's QNAME.
	if seg := uplinkSegment(c.zone); len(target) > seg {
		return nil, fmt.Errorf("dnstunnel: target %d bytes exceeds the %d-byte uplink capacity for this zone", len(target), seg)
	}
	p, err := c.currentPump(ctx)
	if err != nil {
		return nil, err
	}
	return p.open(ctx, target, addr)
}

// NewRoundTripper establishes the tunnel session (bounded by ctx) and returns an HTTP transport whose
// connections ride the tunnel. TLS runs end to end through the tunnel, so the exit only sees
// ciphertext. addr is unused: each request dials its own host.
func (c *Client) NewRoundTripper(ctx context.Context, _ string) (http.RoundTripper, error) {
	p, err := c.currentPump(ctx)
	if err != nil {
		return nil, err
	}
	if err := p.awaitEstablished(ctx); err != nil {
		return nil, err
	}
	return &http.Transport{
		DialContext:         c.DialContext,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		// Short, so an idle keep-alive doesn't hold the session (and its socket) open.
		IdleConnTimeout:     2 * time.Second,
		TLSHandshakeTimeout: 60 * time.Second,
	}, nil
}

// MaxLength caps request bodies: the tunnel moves KB/s, so large uploads belong elsewhere.
func (c *Client) MaxLength() int { return maxReqLength }

// Close tears down any running session. The Client cannot be used afterwards.
func (c *Client) Close() error {
	c.mu.Lock()
	p := c.pump
	c.pump, c.closed = nil, true
	c.mu.Unlock()
	if p != nil {
		p.stop()
	}
	return nil
}

// currentPump returns the running session's pump, starting one if none is live.
func (c *Client) currentPump(ctx context.Context) (*pump, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, net.ErrClosed
	}
	if c.pump != nil && !c.pump.isDone() {
		return c.pump, nil
	}
	pool, err := parseResolvers(c.resolvers, c.cfg.Duplication)
	if err != nil {
		return nil, err
	}
	sess, err := newClientSession(c.serverPub, c.zone, &c.cfg)
	if err != nil {
		return nil, err
	}
	pc, err := c.cfg.ListenPacket(ctx)
	if err != nil {
		return nil, fmt.Errorf("dnstunnel: opening UDP socket: %w", err)
	}
	c.pump = startPump(sess, pool, pc, &c.cfg)
	return c.pump, nil
}

type openReq struct {
	ctx    context.Context
	target []byte
	conn   *tunnelConn
	result chan error
}

type writeReq struct {
	conn   *tunnelConn
	data   []byte
	result chan bool
}

type answerMsg struct {
	from netip.AddrPort
	body []byte
}

// sentQuery tracks one query sent to one or more resolvers. Only answers from those resolvers are
// accepted, each at most once, so a spoofed packet can neither free query slots nor sway the pool.
type sentQuery struct {
	targets  []netip.AddrPort
	answered []bool
	sentAt   uint64
	// probe marks an MTU probe: an oversized one failing is expected, so it never counts against
	// (or for) a resolver.
	probe bool
}

// probeSizes are the downlink payload sizes tried after the handshake; the largest that returns is
// sent to the server with SetMtu, lowering it below the default when a resolver truncates.
var probeSizes = []uint16{400, 600, 800, 1000, 1200}

// pump owns one session: the socket, the session state machine, and every stream's protocol
// state. Everything below runs on the pump goroutine except the socket reader.
type pump struct {
	cfg  *Config
	sess *clientSession
	pool *resolverPool
	pc   net.PacketConn
	log  *slog.Logger

	start time.Time

	openCh   chan *openReq
	writeCh  chan *writeReq
	closeCh  chan *tunnelConn
	answerCh chan answerMsg
	wakeCh   chan struct{}
	estCh    chan struct{} // closed once the handshake completes
	done     chan struct{}
	stopOnce sync.Once

	// Pump-goroutine state.
	conns      map[uint16]*tunnelConn
	pendOpen   map[uint16]*openReq
	pending    map[uint16]*sentQuery
	closing    map[uint16]uint64 // streams the app closed → when
	estWaiters []context.Context

	probeSent, probeDone bool
	probeBest            uint16
	probeDeadline        uint64
	probeGot             map[uint16]bool // probe sizes confirmed by a valid response
	mtuTxn               uint16
	mtuInFlight          bool // a SetMtu awaits its answer; stream data is held until it lands
	lastActive           uint64
}

func startPump(sess *clientSession, pool *resolverPool, pc net.PacketConn, cfg *Config) *pump {
	p := &pump{
		cfg:      cfg,
		sess:     sess,
		pool:     pool,
		pc:       pc,
		log:      cfg.Logger,
		start:    time.Now(),
		openCh:   make(chan *openReq),
		writeCh:  make(chan *writeReq),
		closeCh:  make(chan *tunnelConn, 16),
		answerCh: make(chan answerMsg, answerQueue),
		wakeCh:   make(chan struct{}, 1),
		estCh:    make(chan struct{}),
		done:     make(chan struct{}),
		conns:    make(map[uint16]*tunnelConn),
		pendOpen: make(map[uint16]*openReq),
		pending:  make(map[uint16]*sentQuery),
		closing:  make(map[uint16]uint64),
		probeGot: make(map[uint16]bool),
	}
	go p.readLoop()
	go p.run()
	return p
}

func (p *pump) now() uint64 { return uint64(time.Since(p.start).Milliseconds()) }

func (p *pump) isDone() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *pump) stop() {
	p.stopOnce.Do(func() {
		close(p.done)
		_ = p.pc.Close()
	})
}

func (p *pump) kick() {
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

func (p *pump) awaitEstablished(ctx context.Context) error {
	select {
	case <-p.estCh:
		return nil
	default:
	}
	// Count as activity so the pump doesn't idle out while a caller waits for the handshake.
	select {
	case p.openCh <- &openReq{ctx: ctx}:
	case <-p.done:
		return net.ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-p.estCh:
		return nil
	case <-p.done:
		return net.ErrClosed
	case <-ctx.Done():
		return fmt.Errorf("dnstunnel: handshake: %w", ctx.Err())
	}
}

func (p *pump) open(ctx context.Context, target []byte, remote string) (net.Conn, error) {
	req := &openReq{ctx: ctx, target: target, conn: newTunnelConn(p, remote), result: make(chan error, 1)}
	select {
	case p.openCh <- req:
	case <-p.done:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case err := <-req.result:
		if err != nil {
			return nil, err
		}
		return req.conn, nil
	case <-p.done:
		return nil, net.ErrClosed
	case <-ctx.Done():
		// The pump prunes the abandoned open, or, if it opened in the same instant, the late
		// success is closed here so the stream isn't leaked.
		go func() {
			if err := <-req.result; err == nil {
				_ = req.conn.Close()
			}
		}()
		return nil, fmt.Errorf("dnstunnel: stream open: %w", ctx.Err())
	}
}

// write offers data to the pump; false means the stream is backlogged and the data was not taken.
// A deadline bounds the hand-off; once the pump takes the request it replies without blocking.
func (p *pump) write(c *tunnelConn, data []byte, deadline time.Time) (bool, error) {
	var expired <-chan time.Time
	if !deadline.IsZero() {
		t := time.NewTimer(time.Until(deadline))
		defer t.Stop()
		expired = t.C
	}
	req := &writeReq{conn: c, data: data, result: make(chan bool, 1)}
	select {
	case p.writeCh <- req:
	case <-expired:
		return false, os.ErrDeadlineExceeded // not handed off, so nothing was consumed
	case <-c.closedCh:
		return false, net.ErrClosed
	case <-p.done:
		return false, net.ErrClosed
	}
	select {
	case ok := <-req.result:
		return ok, nil
	case <-p.done:
		return false, net.ErrClosed
	}
}

func (p *pump) closeStream(c *tunnelConn) {
	select {
	case p.closeCh <- c:
	case <-p.done:
	}
}

func (p *pump) readLoop() {
	buf := make([]byte, udpReadBuf)
	for {
		n, from, err := p.pc.ReadFrom(buf)
		if err != nil {
			if p.isDone() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue // a stale ICMP error from one dead resolver must not kill the session
		}
		ua, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		msg := answerMsg{from: ua.AddrPort(), body: append([]byte(nil), buf[:n]...)}
		msg.from = netip.AddrPortFrom(msg.from.Addr().Unmap(), msg.from.Port())
		select {
		case p.answerCh <- msg:
		case <-p.done:
			return
		default:
			// Queue full: dropping an answer is safe, ARQ re-sends on RTO.
		}
	}
}

func (p *pump) run() {
	defer p.shutdown()
	ticker := time.NewTicker(pumpTick)
	defer ticker.Stop()
	for {
		now := p.now()
		p.expireQueries(now)
		p.flushQueries(now)
		p.probeMTU(now)
		p.fanOut()
		if p.idle(now) {
			return
		}
		select {
		case <-p.done:
			return
		case req := <-p.openCh:
			p.handleOpen(req)
		case req := <-p.writeCh:
			p.handleWrite(req)
		case c := <-p.closeCh:
			p.handleClose(c)
		case msg := <-p.answerCh:
			p.handleAnswer(msg, p.now())
			// Drain whatever else is ready before re-polling.
			for drained := false; !drained; {
				select {
				case m := <-p.answerCh:
					p.handleAnswer(m, p.now())
				default:
					drained = true
				}
			}
		case <-p.wakeCh:
		case <-ticker.C:
		}
	}
}

func (p *pump) shutdown() {
	p.stop()
	for _, c := range p.conns {
		c.fail(net.ErrClosed)
	}
	for _, req := range p.pendOpen {
		req.result <- net.ErrClosed
	}
	p.log.Debug("dnstunnel: session closed")
}

func (p *pump) handleOpen(req *openReq) {
	p.lastActive = p.now()
	if req.target == nil {
		p.estWaiters = append(p.estWaiters, req.ctx) // a handshake waiter
		return
	}
	sid, err := p.sess.openStream(req.target)
	if err != nil {
		req.result <- err
		return
	}
	req.conn.sid = sid
	p.pendOpen[sid] = req
}

func (p *pump) handleWrite(req *writeReq) {
	st := p.sess.stream(req.conn.sid)
	if st == nil || st.arq.isClosed() {
		req.result <- true // stream gone; the conn's own error state reports it
		return
	}
	if st.arq.unsent() >= maxUnsent {
		req.result <- false
		return
	}
	st.arq.write(req.data)
	req.result <- true
}

func (p *pump) handleClose(c *tunnelConn) {
	if st := p.sess.stream(c.sid); st != nil {
		st.arq.close()
		p.closing[c.sid] = p.now()
	}
	delete(p.conns, c.sid)
}

func (p *pump) handleAnswer(msg answerMsg, now uint64) {
	from, body := msg.from, msg.body
	txn, ok := txnOf(body)
	if !ok {
		return
	}
	q := p.pending[txn]
	if q == nil {
		return
	}
	i := slices.Index(q.targets, from)
	if i < 0 || q.answered[i] {
		return
	}
	q.answered[i] = true
	if q.probe {
		if !slices.Contains(q.answered, false) {
			delete(p.pending, txn)
		}
		// Only an authenticated probe response counts; an error or empty answer says nothing.
		if size := p.sess.onAnswer(body, now); size > 0 && !p.probeDone {
			p.probeGot[size] = true
			p.probeBest = max(p.probeBest, size)
		}
		return
	}
	// An error rcode (SERVFAIL, REFUSED, ...) means this resolver won't carry the tunnel: count it as
	// a loss so the pool fails over, and don't feed it to the session.
	if len(body) >= 4 && body[3]&0x0F != 0 {
		p.pool.onLoss([]netip.AddrPort{from}, now)
		if !slices.Contains(q.answered, false) {
			delete(p.pending, txn)
			delete(p.sess.outstanding, txn) // no resolver will answer it usefully; free the slot
			if p.mtuInFlight && txn == p.mtuTxn {
				p.sendSetMtu(now) // every resolver refused it: try again (the pool has failed over)
			}
		}
		return
	}
	p.pool.onSuccess(from, now-min(now, q.sentAt))
	if p.mtuInFlight && txn == p.mtuTxn {
		p.mtuInFlight, p.sess.holdData = false, false // the server has applied it
	}
	// Keep tracking duplicates only while the table is small; past that, an answered query is
	// dropped so a dead duplicate resolver can't grow it without bound.
	if !slices.Contains(q.answered, false) || len(p.pending) > p.pendingCap() {
		delete(p.pending, txn)
	}
	wasEst := p.sess.established()
	p.sess.onAnswer(body, now)
	if !wasEst && p.sess.established() {
		// Gate opens and data before anything else goes out, so no stream sees a downlink segment
		// cut before the probed MTU is in place.
		p.sess.holdOpens, p.sess.holdData = true, true
		close(p.estCh)
		p.estWaiters = nil
		p.log.Debug("dnstunnel: session established")
	}
	opened := false
	for sid, req := range p.pendOpen {
		st := p.sess.stream(sid)
		if st == nil || !st.openAcked {
			continue
		}
		delete(p.pendOpen, sid)
		opened = true
		if err := req.ctx.Err(); err != nil {
			st.arq.reset()
			req.result <- err
			continue
		}
		p.conns[sid] = req.conn
		req.result <- nil
	}
	// The server applies SetMtu only to streams that exist, so re-send it as each stream opens. It
	// lands before the stream's first downlink segment: origin data can't exist until our request
	// goes up, and the server cuts segments lazily.
	if opened && p.probeBest > 0 {
		p.sendSetMtu(now)
	}
}

func (p *pump) pendingCap() int { return 4 * p.cfg.MaxQueriesInFlight }

// probeMTU runs one round of downlink MTU probes after the handshake, then tells the server the
// largest size that survived the path.
func (p *pump) probeMTU(now uint64) {
	if p.probeDone || !p.sess.established() {
		return
	}
	if !p.probeSent {
		p.probeSent, p.probeDeadline = true, now+probeWindow
		for _, size := range probeSizes {
			p.sendControl(p.sess.controlQuery(kindMtuProbe, size), now, true)
		}
		return
	}
	// Done once every size is confirmed, or at the deadline (an oversized probe never returns).
	if len(p.probeGot) < len(probeSizes) && now < p.probeDeadline {
		return
	}
	p.probeDone = true
	p.sess.holdOpens = false
	if p.probeBest > 0 {
		p.sendSetMtu(now) // releases data once the server answers it
		p.log.Debug("dnstunnel: downlink MTU set", "bytes", p.probeBest)
	} else {
		p.sess.holdData = false // nothing came back: fall back to the server's default
	}
}

// sendSetMtu tells the server the probed downlink size and holds stream data until it's answered.
func (p *pump) sendSetMtu(now uint64) {
	q := p.sess.controlQuery(kindSetMtu, p.probeBest)
	if q == nil {
		return
	}
	p.sendControl(q, now, false)
	p.mtuTxn, _ = txnOf(q)
	p.mtuInFlight, p.sess.holdData = true, true
}

// send writes a query to each target. A short socket deadline keeps the pump from ever blocking on
// a full send buffer; a dropped datagram is just loss, which ARQ already handles. Per-send errors are
// non-fatal.
func (p *pump) send(q []byte, targets []netip.AddrPort) {
	_ = p.pc.SetWriteDeadline(time.Now().Add(sendTimeout))
	for _, t := range targets {
		_, _ = p.pc.WriteTo(q, net.UDPAddrFromAddrPort(t))
	}
}

func (p *pump) sendControl(q []byte, now uint64, probe bool) {
	if q == nil {
		return
	}
	targets := p.pool.pick(now)
	p.send(q, targets)
	if txn, ok := txnOf(q); ok {
		p.pending[txn] = &sentQuery{targets: targets, answered: make([]bool, len(targets)), sentAt: now, probe: probe}
	}
}

func (p *pump) expireQueries(now uint64) {
	timeout := uint64(p.cfg.QueryTimeout.Milliseconds())
	resendMtu := false
	defer func() {
		if resendMtu {
			p.sendSetMtu(now) // the SetMtu was lost: send it again
		}
	}()
	for txn, q := range p.pending {
		if now-min(now, q.sentAt) >= timeout {
			delete(p.pending, txn)
			if q.probe {
				continue
			}
			if p.mtuInFlight && txn == p.mtuTxn {
				resendMtu = true
			}
			var lost []netip.AddrPort
			for i, t := range q.targets {
				if !q.answered[i] {
					lost = append(lost, t)
				}
			}
			p.pool.onLoss(lost, now)
		}
	}
	// Drop handshake waiters that gave up, so an unreachable server stops being retried.
	p.estWaiters = slices.DeleteFunc(p.estWaiters, func(ctx context.Context) bool { return ctx.Err() != nil })
	// Abandoned opens: free the stream so it stops costing queries.
	for sid, req := range p.pendOpen {
		if req.ctx.Err() != nil {
			delete(p.pendOpen, sid)
			if st := p.sess.stream(sid); st != nil {
				st.arq.reset()
			}
			req.result <- req.ctx.Err()
		}
	}
}

func (p *pump) flushQueries(now uint64) {
	for {
		q := p.sess.pollQuery(now)
		if q == nil {
			return
		}
		targets := p.pool.pick(now)
		p.send(q, targets)
		if txn, ok := txnOf(q); ok {
			p.pending[txn] = &sentQuery{targets: targets, answered: make([]bool, len(targets)), sentAt: now}
		}
	}
}

// fanOut moves delivered bytes into each conn (bounded by the conn's read room) and propagates
// remote close and reset.
func (p *pump) fanOut() {
	for sid, c := range p.conns {
		st := p.sess.stream(sid)
		if st == nil {
			c.fail(errStreamReset)
			delete(p.conns, sid)
			continue
		}
		if room := c.readRoom(); room > 0 && len(st.arq.delivered) > 0 {
			n := min(room, len(st.arq.delivered))
			c.deliver(st.arq.delivered[:n])
			st.arq.delivered = st.arq.delivered[n:]
			if len(st.arq.delivered) == 0 {
				st.arq.delivered = nil
			}
		}
		if st.arq.state == stateReset {
			c.fail(errStreamReset)
			delete(p.conns, sid)
			continue
		}
		if st.arq.remoteFinRecvd && len(st.arq.delivered) == 0 {
			c.setEOF()
		}
		if st.arq.unsent() < maxUnsent {
			select {
			case c.writable <- struct{}{}:
			default:
			}
		}
	}
	for sid, closedAt := range p.closing {
		st := p.sess.stream(sid)
		if st == nil || st.arq.isClosed() {
			delete(p.closing, sid)
			continue
		}
		// Nobody will read this: discard it so the ARQ keeps accepting and the remote FIN advances.
		st.arq.delivered = nil
		if p.now()-min(p.now(), closedAt) >= closeLinger {
			st.arq.reset()
			delete(p.closing, sid)
		}
	}
	p.sess.reapClosed()
}

// idle reports whether the session has had nothing to do for the idle timeout.
func (p *pump) idle(now uint64) bool {
	if len(p.sess.streams) > 0 || len(p.conns) > 0 || len(p.pendOpen) > 0 ||
		(len(p.estWaiters) > 0 && !p.sess.established()) {
		p.lastActive = now
		return false
	}
	return now-min(now, p.lastActive) >= uint64(p.cfg.IdleTimeout.Milliseconds())
}
