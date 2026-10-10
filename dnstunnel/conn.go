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

	// writable is signalled by the pump when the stream's unsent backlog drains below the cap.
	writable chan struct{}
}

func newTunnelConn(p *pump, remote string) *tunnelConn {
	c := &tunnelConn{p: p, remote: remote, writable: make(chan struct{}, 1)}
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
		n := min(len(b)-written, writeChunk)
		ok, perr := c.p.write(c, b[written:written+n])
		if perr != nil {
			return written, perr
		}
		if ok {
			written += n
			continue
		}
		// Backlogged: wait for the pump to drain, the deadline, or a failure.
		var timer *time.Timer
		var timeout <-chan time.Time
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return written, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			timeout = timer.C
		}
		select {
		case <-c.writable:
		case <-timeout:
			return written, os.ErrDeadlineExceeded
		case <-c.p.done:
			return written, net.ErrClosed
		}
		if timer != nil {
			timer.Stop()
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
		c.rtimer = time.AfterFunc(time.Until(t), c.cond.Broadcast)
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
	if !t.IsZero() {
		c.wtimer = time.AfterFunc(time.Until(t), func() {
			select {
			case c.writable <- struct{}{}:
			default:
			}
		})
	}
	c.mu.Unlock()
	return nil
}
