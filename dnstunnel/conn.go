package dnstunnel

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

var errStreamReset = errors.New("dnstunnel: stream reset by peer")

// tunnelAddr is the net.Addr reported for tunnel streams.
type tunnelAddr string

func (a tunnelAddr) Network() string { return "dnstunnel" }
func (a tunnelAddr) String() string  { return string(a) }

// tunnelConn is one multiplexed stream as a net.Conn. The pump goroutine owns the protocol state;
// the conn only holds a bounded read buffer and hands writes to the pump.
type tunnelConn struct {
	p      *pump
	sid    uint16
	remote string

	mu       sync.Mutex
	cond     *sync.Cond
	rbuf     []byte
	eof      bool  // remote half closed and all bytes delivered
	err      error // terminal error (reset, session gone)
	closed   bool
	rdl, wdl time.Time
	rtimer   *time.Timer
	wtimer   *time.Timer

	// writable is signalled when a blocked Write should re-check: the backlog drained, the write
	// deadline changed or passed, or the stream failed.
	writable chan struct{}
	closedCh chan struct{} // closed by Close, unblocking any pending Write
}

func newTunnelConn(p *pump, remote string) *tunnelConn {
	c := &tunnelConn{p: p, remote: remote, writable: make(chan struct{}, 1), closedCh: make(chan struct{})}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *tunnelConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.rbuf) == 0 {
		switch {
		case c.closed:
			return 0, net.ErrClosed
		case c.err != nil:
			return 0, c.err
		case c.eof:
			return 0, io.EOF
		case !c.rdl.IsZero() && !time.Now().Before(c.rdl):
			return 0, os.ErrDeadlineExceeded
		}
		c.cond.Wait()
	}
	n := copy(b, c.rbuf)
	c.rbuf = c.rbuf[n:]
	if len(c.rbuf) == 0 {
		c.rbuf = nil
	}
	c.p.kick()
	return n, nil
}

// readRoom is how many more bytes the pump may deliver into this conn.
func (c *tunnelConn) readRoom() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0
	}
	return connReadBuf - len(c.rbuf)
}

func (c *tunnelConn) deliver(b []byte) {
	c.mu.Lock()
	c.rbuf = append(c.rbuf, b...)
	c.mu.Unlock()
	c.cond.Broadcast()
}

func (c *tunnelConn) setEOF() {
	c.mu.Lock()
	c.eof = true
	c.mu.Unlock()
	c.cond.Broadcast()
}

func (c *tunnelConn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.cond.Broadcast()
	select {
	case c.writable <- struct{}{}:
	default:
	}
}

func (c *tunnelConn) Write(b []byte) (int, error) {
	written := 0
	for written < len(b) {
		c.mu.Lock()
		closed, err, dl := c.closed, c.err, c.wdl
		c.mu.Unlock()
		if closed {
			return written, net.ErrClosed
		}
		if err != nil {
			return written, err
		}
		if !dl.IsZero() && !time.Now().Before(dl) {
			return written, os.ErrDeadlineExceeded
		}
		n := min(len(b)-written, writeChunk)
		ok, perr := c.p.write(c, b[written:written+n])
		if perr != nil {
			return written, perr
		}
		if ok {
			written += n
			continue
		}
		// Backlogged: wait for a reason to re-check (drain, deadline change or expiry, failure),
		// then loop, which re-reads the current deadline.
		select {
		case <-c.writable:
		case <-c.closedCh:
			return written, net.ErrClosed
		case <-c.p.done:
			return written, net.ErrClosed
		}
	}
	return written, nil
}

func (c *tunnelConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.rbuf = nil
	close(c.closedCh)
	c.mu.Unlock()
	c.cond.Broadcast()
	c.p.closeStream(c)
	return nil
}

func (c *tunnelConn) LocalAddr() net.Addr  { return tunnelAddr("dnstunnel") }
func (c *tunnelConn) RemoteAddr() net.Addr { return tunnelAddr(c.remote) }

func (c *tunnelConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *tunnelConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rdl = t
	if c.rtimer != nil {
		c.rtimer.Stop()
		c.rtimer = nil
	}
	if !t.IsZero() {
		// Broadcast under the lock so it can't slip between Read's deadline check and its Wait.
		c.rtimer = time.AfterFunc(time.Until(t), func() {
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
		})
	}
	c.cond.Broadcast()
	return nil
}

func (c *tunnelConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wdl = t
	if c.wtimer != nil {
		c.wtimer.Stop()
		c.wtimer = nil
	}
	poke := func() {
		select {
		case c.writable <- struct{}{}:
		default:
		}
	}
	if !t.IsZero() {
		c.wtimer = time.AfterFunc(time.Until(t), poke)
	}
	c.mu.Unlock()
	poke() // a blocked Write re-evaluates against the new deadline
	return nil
}
