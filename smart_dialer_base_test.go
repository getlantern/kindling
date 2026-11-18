package kindling

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.getoutline.org/sdk/transport"
)

var errBaseRefused = errors.New("base dial refused")

// recordingBase is a stream and packet dialer that records every address the
// smart dialer's strategies dial through it.
type recordingBase struct {
	mu    sync.Mutex
	addrs []string
}

func (d *recordingBase) record(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs = append(d.addrs, addr)
}

func (d *recordingBase) dialed(addr string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.addrs, addr)
}

func (d *recordingBase) DialStream(_ context.Context, addr string) (transport.StreamConn, error) {
	d.record(addr)
	return nil, errBaseRefused
}

func (d *recordingBase) DialPacket(_ context.Context, addr string) (net.Conn, error) {
	d.record(addr)
	return nil, errBaseRefused
}

// TestNewSmartDialerUsesBaseDialers checks that the strategy search dials
// through the stream and packet dialers it is given, as radiance's bypass
// dialer relies on to keep kindling traffic out of its own tunnel.
func TestNewSmartDialerUsesBaseDialers(t *testing.T) {
	config := []byte(`
dns:
  - https: {name: dns.example, address: "192.0.2.1:443"}
  - udp: {address: "192.0.2.2:53"}
tls:
  - ""
`)
	base := &recordingBase{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := newSmartDialer(ctx, io.Discard, config, base, base, "example.com")
	require.Error(t, err, "every base dial is refused, so no strategy can work")
	require.True(t, base.dialed("192.0.2.1:443"), "the DoH resolver must dial through the stream base")
	require.True(t, base.dialed("192.0.2.2:53"), "the UDP resolver must dial through the packet base")
}

// TestNewSmartDialerListenerStrategyRespectsCustomPacketDialer checks that a
// strategy built on a packet listener, such as an h3connect fallback, can't
// open its own socket around a custom packet dialer.
func TestNewSmartDialerListenerStrategyRespectsCustomPacketDialer(t *testing.T) {
	config := []byte(`
dns:
  - https: {name: dns.example, address: "192.0.2.1:443"}
tls:
  - ""
fallback:
  - h3connect://192.0.2.9:443
`)
	base := &recordingBase{}
	var log syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := newSmartDialer(ctx, &log, config, base, base, "example.com")
	require.Error(t, err)
	// The fallback race reports only that every fallback failed; why is in
	// the finder's log.
	require.Contains(t, log.String(), "packet listener strategies are unavailable",
		"an h3connect fallback must fail rather than dial around the custom packet dialer")
}

// syncBuffer is a bytes.Buffer safe for the finder's concurrent log writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
