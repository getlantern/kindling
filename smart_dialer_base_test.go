package kindling

import (
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
